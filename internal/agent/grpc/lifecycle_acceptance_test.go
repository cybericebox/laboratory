package grpc

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	typed "github.com/cybericebox/laboratory/clientset/client/versioned/typed/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestLifecycleAcceptance(t *testing.T) {
	h, k := newTestHandler(t)
	h.features.Limits = limits.Limits{DeviceDefaultCPU: 75, DeviceDefaultMemory: 96 << 20}
	ctx := context.Background()
	readyGroup(t, h, k, "life", "life", nil)
	create := &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web")}}, Items: []*protobuf.LabItem{{LabGroup: "life", Name: "l", VariantId: "v", Env: []*protobuf.DeviceEnv{envOf("web", "FLAG", "original")}}}}
	got, err := h.CreateLabs(ctx, create)
	wantStates(t, got, err, stCreated)
	labs := h.cs.LaboratoryV1alpha1().Labs("life")
	current, err := labs.Get(ctx, "l", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := &protobuf.LabLifecycleTarget{Ref: &protobuf.ItemRef{LabGroup: "life", Name: "l"}, ExpectedLabUid: string(current.UID), OperationId: "stop1", LifecycleRevision: 1}
	stop := &protobuf.StopLabsRequest{Items: []*protobuf.StopLabItem{{Target: target, SnapshotMode: protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_SKIP}}}
	got, err = h.StopLabs(ctx, stop)
	wantStates(t, got, err, stUpdated)
	stopped, _ := labs.Get(ctx, "l", metav1.GetOptions{})
	if stopped.Spec.Lifecycle == nil || !stopped.Spec.Lifecycle.IsStopped() {
		t.Fatal("stop intent was not persisted")
	}
	// Accepted intent cannot manufacture completion or release.
	listed, err := h.ListLabs(ctx, &protobuf.ListRequest{LabGroup: "life"})
	if err != nil {
		t.Fatal(err)
	}
	p := listed.Items[0]
	if p.Status.Lifecycle.ObservedState != "Unknown" || p.Status.Resources.GetRuntimeState() != "Unknown" || p.Status.Resources.GetAllocatedRequests().GetMemoryBytes() <= 0 {
		t.Fatalf("acceptance claimed completion/free capacity: %v", p.Status)
	}
	monitor, err := h.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(monitor.Labs) != 1 || len(monitor.Labs[0].SpecJson) != 0 || !proto.Equal(p.Status.Lifecycle, monitor.Labs[0].Status.Lifecycle) || !proto.Equal(p.Status.Resources, monitor.Labs[0].Status.Resources) {
		t.Fatalf("List/Monitoring observations differ or expose spec: %v", monitor.Labs)
	}
	// A status advance must not make an identical retry fail or rewrite the object.
	stopped.Status.Lifecycle = &lab.LabLifecycleStatus{LabUID: string(stopped.UID), OperationID: "stop1", Revision: 1, ObservedGeneration: stopped.Generation, ObservedState: "Stopping"}
	stopped, err = labs.UpdateStatus(ctx, stopped, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err = h.StopLabs(ctx, stop)
	wantStates(t, got, err, stUpdated)
	after, _ := labs.Get(ctx, "l", metav1.GetOptions{})
	if after.ResourceVersion != stopped.ResourceVersion {
		t.Fatal("idempotent retry mutated accepted intent")
	}
	for _, tc := range []struct {
		name string
		mut  func(*protobuf.StopLabsRequest)
	}{
		{"missing UID", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.ExpectedLabUid = "" }},
		{"missing operation", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.OperationId = " " }},
		{"missing ref", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.Ref = nil }},
		{"ambiguous ref", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.Ref.Lab = "extra" }},
		{"unset policy", func(r *protobuf.StopLabsRequest) {
			r.Items[0].SnapshotMode = protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_UNSPECIFIED
		}},
		{"negative retention", func(r *protobuf.StopLabsRequest) { r.Items[0].RetentionUntilUnixMs = -1 }},
		{"wrong UID", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.ExpectedLabUid = "recreated" }},
		{"equal conflict", func(r *protobuf.StopLabsRequest) { r.Items[0].Terminal = true }},
		{"lower revision", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.LifecycleRevision = 0 }},
		{"wrong op", func(r *protobuf.StopLabsRequest) { r.Items[0].Target.OperationId = "other" }},
		{"required unsupported", func(r *protobuf.StopLabsRequest) {
			r.Items[0].Target.LifecycleRevision = 2
			r.Items[0].Target.OperationId = "required"
			r.Items[0].SnapshotMode = protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_REQUIRED
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proto.Clone(stop).(*protobuf.StopLabsRequest)
			tc.mut(r)
			got, err := h.StopLabs(ctx, r)
			wantStates(t, got, err, stFailed)
		})
	}
	t.Run("duplicate and bound", func(t *testing.T) {
		r := proto.Clone(stop).(*protobuf.StopLabsRequest)
		r.Items = append(r.Items, r.Items[0])
		_, err := h.StopLabs(ctx, r)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("duplicate: %v", err)
		}
		r.Items = make([]*protobuf.StopLabItem, 5001)
		_, err = h.StopLabs(ctx, r)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("bound: %v", err)
		}
	})
	// Repeated Create never clears stopped intent or rewrites its secrets.
	create.Items[0].Env = []*protobuf.DeviceEnv{envOf("web", "FLAG", "replacement")}
	got, err = h.CreateLabs(ctx, create)
	wantStates(t, got, err, stExists)
	secretName, err := h.deviceObjectName(ctx, "life", "l", "web")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := k.CoreV1().Secrets("life").Get(ctx, secretName+"-env", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(sec.Data["FLAG"]) != "original" {
		t.Fatalf("repeat create rewrote stopped secret: %v", sec.Data)
	}
	start := &protobuf.StartLabsRequest{Items: []*protobuf.LabLifecycleTarget{proto.Clone(target).(*protobuf.LabLifecycleTarget)}}
	start.Items[0].OperationId = "start2"
	start.Items[0].LifecycleRevision = 2
	got, err = h.StartLabs(ctx, start)
	wantStates(t, got, err, stUpdated)
	got, err = h.StartLabs(ctx, start)
	wantStates(t, got, err, stUpdated)
	stop.Items[0].Target.OperationId = "solved"
	stop.Items[0].Target.LifecycleRevision = 3
	stop.Items[0].Terminal = true
	got, err = h.StopLabs(ctx, stop)
	wantStates(t, got, err, stUpdated)
	start.Items[0].OperationId = "forbidden"
	start.Items[0].LifecycleRevision = 4
	got, err = h.StartLabs(ctx, start)
	wantStates(t, got, err, stFailed)
	// Termination is checked even for an otherwise identical accepted retry.
	after, _ = labs.Get(ctx, "l", metav1.GetOptions{})
	after.Finalizers = []string{"test/hold"}
	after, err = labs.Update(ctx, after, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = labs.Delete(ctx, "l", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err = h.StopLabs(ctx, stop)
	wantStates(t, got, err, stFailed)
	if !got.Results[0].Retryable {
		t.Fatal("terminating failure must be retryable")
	}
}

func TestLifecycleProjectionRejectsStaleRelease(t *testing.T) {
	raw := specJSON("web")
	var spec lab.LabSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid"), Generation: 3}, Spec: spec}
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "current", Revision: 2, SnapshotMode: "Skip"}
	l.Status.Lifecycle = &lab.LabLifecycleStatus{ObservedState: "Stopped", LabUID: "uid", OperationID: "old", Revision: 1, ObservedGeneration: 2}
	now := metav1.NewTime(time.Now())
	l.Status.Resources = &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "old", Revision: 1, ObservedAt: &now, ReleasedAt: &now}
	for _, include := range []bool{true, false} {
		p := labProjection(l, include, limits.Limits{DeviceDefaultCPU: 75, DeviceDefaultMemory: 96 << 20})
		if p.Status.Lifecycle.ObservedState != "Unknown" || p.Status.Resources.GetRuntimeState() != "Unknown" || p.Status.Resources.GetReleasedUnixMs() != 0 || p.Status.Resources.GetAllocatedRequests().GetMemoryBytes() <= 0 {
			t.Fatalf("stale release accepted: %v", p.Status)
		}
	}
}

func TestSizingV2Descriptor(t *testing.T) {
	fd := protobuf.File_pkg_agent_protobuf_agent_proto
	for msg, fields := range map[string][]string{"GroupPodsFeature": {"vpn", "gateway", "default_vpn", "default_gateway", "sizing_v2"}, "GroupPodsSizingV2": {"profiles"}, "GroupPodsSizingProfile": {"id", "support_state", "max_inputs", "vpn", "gateway", "validation_provenance", "scope"}, "GroupSizingInputs": {"max_users", "max_active_labs", "internet_labs", "allowed_relations", "envelope"}, "GroupTrafficEnvelope": {"vpn_retained_flows", "gateway_retained_flows", "vpn_new_flows_per_second", "gateway_new_flows_per_second", "vpn_packets_per_second", "gateway_packets_per_second", "vpn_payload_mbps", "gateway_payload_mbps"}, "GroupPodFormula": {"base", "per_user", "per_active_lab", "per_internet_lab", "per_allowed_relation", "per_retained_flow", "floor", "round_to"}} {
		m := fd.Messages().ByName(protoreflect.Name(msg))
		if m == nil {
			t.Fatalf("missing message %s", msg)
		}
		for i, name := range fields {
			f := m.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
			if f == nil || string(f.Name()) != name {
				t.Fatalf("%s tag%d must be %s", msg, i+1, name)
			}
		}
	}

}

// This intercepts one typed update while leaving all version/conflict decisions
// to the real API server. Production must re-read identity/intent after conflict.
type lifecycleConflictClient struct {
	versioned.Interface
	before func() error
}

func (c lifecycleConflictClient) LaboratoryV1alpha1() typed.LaboratoryV1alpha1Interface {
	return lifecycleConflictAPI{c.Interface.LaboratoryV1alpha1(), c.before}
}

type lifecycleConflictAPI struct {
	typed.LaboratoryV1alpha1Interface
	before func() error
}

func (c lifecycleConflictAPI) Labs(ns string) typed.LabInterface {
	return lifecycleConflictLabs{c.LaboratoryV1alpha1Interface.Labs(ns), c.before}
}

type lifecycleConflictLabs struct {
	typed.LabInterface
	before func() error
}

func (c lifecycleConflictLabs) Update(ctx context.Context, l *lab.Lab, o metav1.UpdateOptions) (*lab.Lab, error) {
	if err := c.before(); err != nil {
		return nil, err
	}
	return c.LabInterface.Update(ctx, l, o)
}

func TestLifecycleRPCClientConflictTenantAndRequired(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "rpc-life", "rpc-life", nil)
	h.SetStatePersistence(true)
	// The client reaches the actual registered plural RPC contract over a socket.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := ggrpc.NewServer()
	protobuf.RegisterLabManagerServer(server, h)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	rpc, err := client.NewConnection(client.Config{Endpoint: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rpc.Close() })
	originalCS := h.cs
	labs := originalCS.LaboratoryV1alpha1().Labs("rpc-life")
	makeLab := func(name string, persistent bool) *lab.Lab {
		t.Helper()
		spec := lab.LabSpec{Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer, Image: "nginx"}}}
		if persistent {
			spec.Devices[0].Persistence = &lab.DevicePersistence{Enabled: true}
		}
		l, err := labs.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "rpc-life", Labels: stampTenant(nil, tenantOf(ctx))}, Spec: spec}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	request := func(l *lab.Lab) *client.StopLabsRequest {
		return &client.StopLabsRequest{Items: []*client.StopLabItem{{Target: &client.LabLifecycleTarget{Ref: &protobuf.ItemRef{LabGroup: "rpc-life", Name: l.Name}, ExpectedLabUid: string(l.UID), OperationId: "op", LifecycleRevision: 1}, SnapshotMode: client.StopSnapshotMode_STOP_SNAPSHOT_MODE_SKIP}}}
	}

	t.Run("real conflict retry preserves annotation", func(t *testing.T) {
		l := makeLab("conflict", false)
		h.cs = lifecycleConflictClient{originalCS, onceLifecycleMutation(func() error {
			live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			live.Annotations = map[string]string{"concurrent": "preserved"}
			_, err = labs.Update(ctx, live, metav1.UpdateOptions{})
			return err
		})}
		defer func() { h.cs = originalCS }()
		got, err := rpc.StopLabs(ctx, request(l))
		wantStates(t, got, err, stUpdated)
		live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if live.Annotations["concurrent"] != "preserved" || live.Spec.Lifecycle == nil {
			t.Fatal("conflict retry lost concurrent state")
		}
		start := &client.StartLabsRequest{Items: []*client.LabLifecycleTarget{{Ref: &protobuf.ItemRef{LabGroup: "rpc-life", Name: l.Name}, ExpectedLabUid: string(l.UID), OperationId: "start", LifecycleRevision: 2}}}
		got, err = rpc.StartLabs(ctx, start)
		wantStates(t, got, err, stUpdated)
	})
	t.Run("conflicting newer intent wins", func(t *testing.T) {
		l := makeLab("newer", false)
		h.cs = lifecycleConflictClient{originalCS, onceLifecycleMutation(func() error {
			live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			live.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "newer", Revision: 2, SnapshotMode: "Skip"}
			_, err = labs.Update(ctx, live, metav1.UpdateOptions{})
			return err
		})}
		defer func() { h.cs = originalCS }()
		got, err := rpc.StopLabs(ctx, request(l))
		wantStates(t, got, err, stFailed)
		live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if live.Spec.Lifecycle.OperationID != "newer" {
			t.Fatal("stale retry overwrote current intent")
		}
	})
	t.Run("recreated UID refused after conflict", func(t *testing.T) {
		l := makeLab("recreated", false)
		h.cs = lifecycleConflictClient{originalCS, onceLifecycleMutation(func() error {
			if err := labs.Delete(ctx, l.Name, metav1.DeleteOptions{}); err != nil {
				return err
			}
			_, err := labs.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: l.Name, Namespace: l.Namespace, Labels: l.Labels}, Spec: l.Spec}, metav1.CreateOptions{})
			return err
		})}
		defer func() { h.cs = originalCS }()
		got, err := rpc.StopLabs(ctx, request(l))
		wantStates(t, got, err, stFailed)
		live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if live.UID == l.UID || live.Spec.Lifecycle != nil {
			t.Fatal("replacement lab was mutated")
		}
	})
	t.Run("live namespace owner checked on retry", func(t *testing.T) {
		l := makeLab("group-fence", false)
		groups := originalCS.LaboratoryV1alpha1().LabGroups()
		defer func() {
			h.cs = originalCS
			g, err := groups.Get(ctx, "rpc-life", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			g.Status.Namespace = "rpc-life"
			if _, err = groups.UpdateStatus(ctx, g, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}()
		h.cs = lifecycleConflictClient{originalCS, onceLifecycleMutation(func() error {
			live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			live.Annotations = map[string]string{"concurrent": "owner-change"}
			if _, err = labs.Update(ctx, live, metav1.UpdateOptions{}); err != nil {
				return err
			}
			g, err := groups.Get(ctx, "rpc-life", metav1.GetOptions{})
			if err != nil {
				return err
			}
			g.Status.Namespace = "replacement-namespace"
			_, err = groups.UpdateStatus(ctx, g, metav1.UpdateOptions{})
			return err
		})}
		got, err := rpc.StopLabs(ctx, request(l))
		wantStates(t, got, err, stFailed)
		live, err := labs.Get(ctx, l.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if live.Spec.Lifecycle != nil {
			t.Fatal("stale resolver mutated lab after namespace owner changed")
		}
	})
	t.Run("tenant boundaries", func(t *testing.T) {
		l := makeLab("tenant", false)
		got, err := h.StopLabs(asClient("another-tenant"), request(l))
		wantStates(t, got, err, stNotFound)
		l.Labels[names.LabelTenant] = "another-tenant"
		if _, err = labs.Update(ctx, l, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err = h.StopLabs(ctx, request(l))
		wantStates(t, got, err, stNotFound)
	})
	t.Run("required preparation", func(t *testing.T) {
		l := makeLab("persistent", true)
		r := request(l)
		r.Items[0].SnapshotMode = client.StopSnapshotMode_STOP_SNAPSHOT_MODE_REQUIRED
		got, err := rpc.StopLabs(ctx, r)
		wantStates(t, got, err, stFailed)
		h.SetRequiredSnapshotAvailable(true)
		defer h.SetRequiredSnapshotAvailable(false)
		h.SetStatePersistence(false)
		got, err = rpc.StopLabs(ctx, r)
		wantStates(t, got, err, stFailed)
		h.SetStatePersistence(true)
		// Existing Deployment-backed Device cannot acquire required policy retroactively.
		d, err := h.cs.LaboratoryV1alpha1().Devices(l.Namespace).Create(ctx, &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "persistent-web", Namespace: l.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: l.Name, UID: l.UID}}}, Spec: lab.DeviceSpec{Name: "web", LabRef: l.Name, Type: lab.DeviceTypeContainer, Image: "nginx"}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err = rpc.StopLabs(ctx, r)
		wantStates(t, got, err, stFailed)
		d.Spec.State = &lab.DeviceStateSpec{Enabled: true}
		d, err = h.cs.LaboratoryV1alpha1().Devices(l.Namespace).Update(ctx, d, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err = rpc.StopLabs(ctx, r)
		wantStates(t, got, err, stFailed) // persistence alone is not a native handshake
		now := metav1.Now()
		d.Status.NodeName = "node"
		d.Status.State = &lab.DeviceStateStatus{Epoch: 1, Incarnation: 1}
		d.Status.RuntimeReports = []lab.OwnedRuntimeReport{{Identity: lab.OwnedRuntimeIdentity{OwnerUID: string(l.UID), OperationID: "running", Revision: 1, PodUID: "native-pod", NodeName: "node", NodeBootID: "native-boot", ContainerIDs: []string{"native-container"}, CgroupPaths: []string{"/native-container"}, PortKeys: []string{"native-port"}, Epoch: 1, Incarnation: 1}, RuntimeState: "Present", ObservedAt: &now}}
		if _, err = h.cs.LaboratoryV1alpha1().Devices(l.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err = rpc.StopLabs(ctx, r)
		wantStates(t, got, err, stUpdated)
		features, err := h.GetFeatures(ctx, &protobuf.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		if features.GetLifecycle().GetRequiredSnapshot() || features.GetLifecycle().GetPerLabStop() || features.GetLifecycle().GetFullGroupStop() {
			t.Fatal("acceptance test gate advertised native support")
		}
	})
	t.Run("retention retry semantic time", func(t *testing.T) {
		l := makeLab("retention", false)
		r := request(l)
		r.Items[0].RetentionUntilUnixMs = 1792000000123
		for i := 0; i < 2; i++ {
			got, err := rpc.StopLabs(ctx, r)
			wantStates(t, got, err, stUpdated)
		}
		live, _ := labs.Get(ctx, l.Name, metav1.GetOptions{})
		if live.Spec.Lifecycle.RetentionUntil.UnixMilli() != 1792000001000 {
			t.Fatalf("deadline not conservatively canonicalized: %v", live.Spec.Lifecycle.RetentionUntil)
		}
	})
	t.Run("batch partial validation", func(t *testing.T) {
		l := makeLab("valid", false)
		r := request(l)
		r.Items = append(r.Items, &client.StopLabItem{Target: &client.LabLifecycleTarget{Ref: &protobuf.ItemRef{LabGroup: "rpc-life", Name: "invalid"}}, SnapshotMode: client.StopSnapshotMode_STOP_SNAPSHOT_MODE_SKIP})
		got, err := rpc.StopLabs(ctx, r)
		wantStates(t, got, err, stUpdated, stFailed)
	})
	t.Run("Create cannot bypass required acceptance", func(t *testing.T) {
		spec := lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "bypass", Revision: 1, SnapshotMode: "Required"}, Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer, Image: "nginx"}}}
		raw, _ := json.Marshal(spec)
		_, err := rpc.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: raw}}, Items: []*protobuf.LabItem{{LabGroup: "rpc-life", Name: "bypass", VariantId: "v"}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Create bypassed lifecycle preparation: %v", err)
		}
	})
}

func TestLifecycleReleasedProjectionRequiresCurrentIdentity(t *testing.T) {
	now := metav1.NewTime(time.Unix(1000, 0))
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "u", Generation: 7}, Spec: lab.LabSpec{Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer}}, Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "current", Revision: 4, SnapshotMode: "Skip"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "u", OperationID: "current", Revision: 4, ObservedGeneration: 7, ObservedState: "Stopped"}, Resources: &lab.RuntimeAllocation{OperationID: "current", Revision: 4, RuntimeState: "Released", ObservedAt: &now, ReleasedAt: &now}}}
	sizing := limits.Limits{DeviceDefaultCPU: 75, DeviceDefaultMemory: 96 << 20}
	valid := labProjection(l, false, sizing).Status.Resources
	if valid.RuntimeState != "Released" || valid.AllocatedRequests.GetMemoryBytes() != 0 || valid.ReleasedUnixMs != 1000000 {
		t.Fatalf("current release lost: %v", valid)
	}
	for _, tc := range []struct {
		name string
		mut  func(*lab.Lab)
	}{
		{"UID", func(l *lab.Lab) { l.Status.Lifecycle.LabUID = "old" }},
		{"generation", func(l *lab.Lab) { l.Status.Lifecycle.ObservedGeneration = 6 }},
		{"operation", func(l *lab.Lab) { l.Status.Lifecycle.OperationID = "old" }},
		{"revision", func(l *lab.Lab) { l.Status.Lifecycle.Revision = 3 }},
		{"allocation revision", func(l *lab.Lab) { l.Status.Resources.Revision = 3 }},
		{"allocation op", func(l *lab.Lab) { l.Status.Resources.OperationID = "old" }},
		{"missing lifecycle", func(l *lab.Lab) { l.Status.Lifecycle = nil }},
		{"missing resource", func(l *lab.Lab) { l.Status.Resources = nil }},
		{"missing observation time", func(l *lab.Lab) { l.Status.Resources.ObservedAt = nil }},
		{"missing release time", func(l *lab.Lab) { l.Status.Resources.ReleasedAt = nil }},
		{"pending start", func(l *lab.Lab) { l.Spec.Lifecycle.DesiredState = "Running" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := l.DeepCopy()
			tc.mut(copy)
			got := labProjection(copy, false, sizing).Status.Resources
			if got.RuntimeState != "Unknown" || got.ReleasedUnixMs != 0 || got.AllocatedRequests.GetCpuMillicores() != 75 || got.AllocatedRequests.GetMemoryBytes() != 96<<20 {
				t.Fatalf("inexact release admitted: %v", got)
			}
		})
	}
}

func onceLifecycleMutation(fn func() error) func() error {
	var once sync.Once
	var mutationErr error
	return func() error { once.Do(func() { mutationErr = fn() }); return mutationErr }
}
