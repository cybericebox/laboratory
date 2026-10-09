package grpc

import (
	"context"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// The fixture is protoc output from the pre-lifecycle contract. Every old field,
// reserved tag, enum value and method must keep its existing wire meaning.
func TestLifecycleWireCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/agent-legacy-descriptor.pb")
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	old, err := protodesc.NewFile(set.File[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	current := protobuf.File_pkg_agent_protobuf_agent_proto
	for i := 0; i < old.Messages().Len(); i++ {
		before := old.Messages().Get(i)
		after := current.Messages().ByName(before.Name())
		if after == nil {
			t.Fatalf("lost message %s", before.Name())
		}
		for j := 0; j < before.Fields().Len(); j++ {
			f := before.Fields().Get(j)
			n := after.Fields().ByNumber(f.Number())
			if n == nil || n.Name() != f.Name() || n.Kind() != f.Kind() || n.Cardinality() != f.Cardinality() || n.IsMap() != f.IsMap() {
				t.Fatalf("changed wire field %s.%s (%d)", before.Name(), f.Name(), f.Number())
			}
			if f.Message() != nil && n.Message().FullName() != f.Message().FullName() {
				t.Fatalf("changed field message %s", f.FullName())
			}
			if f.Enum() != nil && n.Enum().FullName() != f.Enum().FullName() {
				t.Fatalf("changed field enum %s", f.FullName())
			}
		}
		for j := 0; j < before.ReservedRanges().Len(); j++ {
			r := before.ReservedRanges().Get(j)
			for tag := r[0]; tag < r[1]; tag++ {
				if !after.ReservedRanges().Has(tag) {
					t.Fatalf("lost reserved tag %s.%d", before.Name(), tag)
				}
			}
		}
	}
	for i := 0; i < old.Enums().Len(); i++ {
		e := old.Enums().Get(i)
		n := current.Enums().ByName(e.Name())
		if n == nil {
			t.Fatalf("lost enum %s", e.Name())
		}
		for j := 0; j < e.Values().Len(); j++ {
			v := e.Values().Get(j)
			w := n.Values().ByNumber(v.Number())
			if w == nil || w.Name() != v.Name() {
				t.Fatalf("changed enum %s.%s", e.Name(), v.Name())
			}
		}
	}
	for i := 0; i < old.Services().Len(); i++ {
		s := old.Services().Get(i)
		n := current.Services().ByName(s.Name())
		if n == nil {
			t.Fatalf("lost service %s", s.Name())
		}
		for j := 0; j < s.Methods().Len(); j++ {
			m := s.Methods().Get(j)
			w := n.Methods().ByName(m.Name())
			if w == nil || w.Input().FullName() != m.Input().FullName() || w.Output().FullName() != m.Output().FullName() || w.IsStreamingClient() != m.IsStreamingClient() || w.IsStreamingServer() != m.IsStreamingServer() {
				t.Fatalf("changed method %s", m.FullName())
			}
		}
	}
	// Hand-encoded legacy Ready, CIDR, readiness, URL and image-warning fields.
	wire := []byte{0x0a, 5, 'R', 'e', 'a', 'd', 'y', 0x12, 2, '1', '0', 0x20, 1, 0x2a, 5, 'h', 't', 't', 'p', 's', 0xc2, 2, 3, 't', 'a', 'g'}
	var legacy protobuf.LabStatus
	if err := proto.Unmarshal(wire, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Phase != "Ready" || legacy.VpnCidr != "10" || !legacy.Ready || len(legacy.AccessUrls) != 1 || legacy.AccessUrls[0] != "https" || legacy.ImageWarning != "tag" {
		t.Fatalf("legacy decode = %v", &legacy)
	}
	round, err := proto.Marshal(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protobuf.LabStatus
	if err := proto.Unmarshal(round, &decoded); err != nil || !proto.Equal(&legacy, &decoded) {
		t.Fatalf("legacy roundtrip = %v, %v", &decoded, err)
	}
}

func TestLifecycleAdditiveWireContract(t *testing.T) {
	file := protobuf.File_pkg_agent_protobuf_agent_proto
	for _, tc := range []struct {
		message, field string
		tag            protoreflect.FieldNumber
	}{
		{"Lab", "uid", 13}, {"LabGroup", "uid", 14}, {"Lab", "generation", 14}, {"LabGroup", "generation", 15}, {"LabStatus", "lifecycle", 41}, {"LabStatus", "resources", 42},
		{"LabGroupStatus", "lifecycle", 9}, {"LabGroupStatus", "resources", 10}, {"FeaturesResponse", "lifecycle", 13},
		{"StopLabItem", "terminal", 4},
		{"LabLifecycleStatus", "access_fenced", 13}, {"LabLifecycleStatus", "access_fenced_unix_ms", 14}, {"LabLifecycleStatus", "access_fence_vpn_boot_id", 15}, {"LabLifecycleStatus", "terminal", 16},
		{"LabGroupAccessPolicy", "operation_id", 5}, {"LabGroupAccessPolicy", "desired_revision", 6}, {"LabGroupAccessPolicy", "generation", 7}, {"LabGroupAccessPolicy", "policy_uid", 8}, {"LabGroupAccessPolicy", "expected_group_uid", 9},
		{"LabGroupAccessPolicyStatus", "applied_revision", 6}, {"LabGroupAccessPolicyStatus", "operation_id", 7}, {"LabGroupAccessPolicyStatus", "vpn_boot_id", 8}, {"LabLifecycleTarget", "expected_lab_uid", 4},
	} {
		m := file.Messages().ByName(protoreflect.Name(tc.message))
		if m == nil {
			t.Errorf("missing message %s", tc.message)
			continue
		}
		f := m.Fields().ByName(protoreflect.Name(tc.field))
		if f == nil || f.Number() != tc.tag {
			t.Errorf("%s.%s must use tag %d", tc.message, tc.field, tc.tag)
		}
	}
	service := file.Services().ByName("LabManager")
	for _, name := range []protoreflect.Name{"StopLabs", "StartLabs"} {
		m := service.Methods().ByName(name)
		if m == nil {
			t.Errorf("missing method %s", name)
			continue
		}
		if m.Output().Name() != "BatchResult" || m.IsStreamingClient() || m.IsStreamingServer() {
			t.Errorf("%s must return unary intent acceptance", name)
		}
		fields := m.Input().Fields()
		if fields.Len() != 1 || fields.Get(0).Name() != "items" || !fields.Get(0).IsList() {
			t.Errorf("%s must have only explicit repeated items", name)
		}
	}
}

func TestLifecycleProjectionSuppressesStoppedAccess(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: "team", UID: types.UID("uid-1")}, Status: lab.LabStatus{
		Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.1.0.0/24"}, Internet: lab.LabNetworkStatus{Ready: true},
		Devices: []lab.DeviceRef{{Name: "web", Ready: true}}, Connections: []lab.ConnectionRef{{Name: "link", Ready: true}},
		Access: []lab.AccessEntry{{Device: "web", Port: 80, Protocol: "http", URL: "https://web.test"}},
	}}
	old := labToProto(l)
	if old.Uid != "uid-1" || !old.Status.Ready || len(old.Status.Access) != 1 || old.Status.Lifecycle != nil || old.Status.Resources != nil {
		t.Fatalf("legacy projection changed: %v", old)
	}
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1, SnapshotMode: "Skip", Terminal: true}
	for _, got := range []*protobuf.Lab{labToProto(l), labMonitoringToProto(l, "group")} {
		s := got.Status
		if got.Uid != "uid-1" || s.Ready || s.VpnReady || s.InternetReady || len(s.Access) != 0 || len(s.AccessUrls) != 0 || s.Devices[0].Ready || s.Connections[0].Ready {
			t.Fatalf("stopped intent exposes readiness/access: %v", got)
		}
		if s.Phase != "Ready" || s.VpnCidr != "10.1.0.0/24" {
			t.Fatal("legacy phase/CIDR must stay intact")
		}
		if s.Lifecycle == nil || s.Lifecycle.DesiredState != "Stopped" || s.Lifecycle.ObservedState != "Unknown" || s.Lifecycle.LabUid != "uid-1" || !s.Lifecycle.Terminal {
			t.Fatalf("missing conservative lifecycle: %v", s.Lifecycle)
		}
	}
	if labMonitoringToProto(l, "group").SpecJson != nil {
		t.Fatal("monitoring leaked spec")
	}
}

func TestLifecycleUnsupportedUntilNativeGates(t *testing.T) {
	h := featuresHandler(t, testFeatures, newTenantTenant("a", true, nil))
	got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	f := got.GetLifecycle()
	if f.GetPerLabStop() || f.GetRequiredSnapshot() || f.GetConfirmedRuntime() || f.GetRetainedRestart() || f.GetFullGroupStop() {
		t.Fatalf("unvalidated lifecycle advertised: %v", f)
	}
	// Acceptance now exists, while advertised native capabilities stay false.
	// Empty batches still fail without mutations.
	if _, err := h.StopLabs(context.Background(), &protobuf.StopLabsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty stop accepted: %v", err)
	}
	if _, err := h.StartLabs(context.Background(), &protobuf.StartLabsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty start accepted: %v", err)
	}
}

func TestLifecycleCRDRejectsStaleConflictAndTerminalStart(t *testing.T) {
	h, k8s := newTestHandler(t)
	mustNamespace(t, k8s, "lifecycle")
	labs := h.cs.LaboratoryV1alpha1().Labs("lifecycle")
	ctx := context.Background()
	current, err := labs.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: "lifecycle"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Adding the first intent is valid for an existing legacy object.
	current.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op-2", Revision: 2, SnapshotMode: "Skip"}
	current, err = labs.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*lab.Lab)
	}{
		{"wrong UID", func(l *lab.Lab) { l.UID = "wrong" }},
		{"lower revision", func(l *lab.Lab) { l.Spec.Lifecycle.Revision = 1 }},
		{"equal conflicting operation", func(l *lab.Lab) { l.Spec.Lifecycle.OperationID = "other" }},
		{"equal conflicting action", func(l *lab.Lab) { l.Spec.Lifecycle.DesiredState = "Running" }},
		{"equal conflicting policy", func(l *lab.Lab) { l.Spec.Lifecycle.SnapshotMode = "Required" }},
		{"remove lifecycle", func(l *lab.Lab) { l.Spec.Lifecycle = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := current.DeepCopy()
			tc.change(next)
			if _, err := labs.Update(ctx, next, metav1.UpdateOptions{}); err == nil || (!apierrors.IsInvalid(err) && !apierrors.IsConflict(err)) {
				t.Fatalf("unsafe update accepted: %v", err)
			}
		})
	}
	if _, err := labs.Update(ctx, current.DeepCopy(), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("identical replay refused: %v", err)
	}
	current.Spec.Lifecycle.Terminal = true
	current.Spec.Lifecycle.Revision = 3
	current.Spec.Lifecycle.OperationID = "solved"
	current, err = labs.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*lab.Lab)
	}{
		{"terminal start", func(l *lab.Lab) { l.Spec.Lifecycle.DesiredState = "Running" }},
		{"terminal clear", func(l *lab.Lab) { l.Spec.Lifecycle.Terminal = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := current.DeepCopy()
			next.Spec.Lifecycle.Revision = 4
			next.Spec.Lifecycle.OperationID = "new"
			tc.change(next)
			if _, err := labs.Update(ctx, next, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
				t.Fatalf("terminal transition accepted: %v", err)
			}
		})
	}
}

func TestLifecycleACLProjectionCarriesFenceIdentity(t *testing.T) {
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{UID: "policy-u", Generation: 7}, Spec: lab.LabGroupAccessPolicySpec{OperationID: "acl", Revision: 3, ExpectedGroupUID: "group-u"}, Status: lab.LabGroupAccessPolicyStatus{State: "Applied", ObservedGeneration: 7, AppliedRevision: 3, OperationID: "acl", VPNBootID: "boot"}}
	got := accessPolicyToProto(policy, "group")
	if got.OperationId != "acl" || got.DesiredRevision != 3 || got.Generation != 7 || got.PolicyUid != "policy-u" || got.ExpectedGroupUid != "group-u" || got.Status.AppliedRevision != 3 || got.Status.OperationId != "acl" || got.Status.VpnBootId != "boot" {
		t.Fatalf("ACL fence identity lost: %v", got)
	}
}

func TestLifecycleProjectionDoesNotReuseOlderObservation(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "lab-u", Generation: 5}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "new", Revision: 2, SnapshotMode: "Required"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{ObservedState: "Stopped", OperationID: "old", Revision: 1, LabUID: "lab-u", ObservedGeneration: 4, SnapshotComplete: true, AccessFenced: true}, Resources: &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "old", Revision: 1}}}
	got := labToProto(l).Status
	if got.Lifecycle.OperationId != "new" || got.Lifecycle.LifecycleRevision != 2 || got.Lifecycle.ObservedState != "Unknown" || got.Lifecycle.AccessFenced || got.Lifecycle.SnapshotComplete {
		t.Fatalf("stale lifecycle acknowledged new intent: %v", got.Lifecycle)
	}
	// Preserve the independently identified allocation; the consumer must compare its
	// revision instead of treating this historical release as the new intent's release.
	if got.Resources.OperationId != "old" || got.Resources.LifecycleRevision != 1 {
		t.Fatal("rewrote stale resource identity")
	}
	l.Status.Lifecycle.OperationID = "new"
	l.Status.Lifecycle.Revision = 2
	l.Status.Lifecycle.ObservedGeneration = 5
	got = labToProto(l).Status
	if got.Lifecycle.ObservedState != "Stopped" || !got.Lifecycle.AccessFenced || !got.Lifecycle.SnapshotComplete {
		t.Fatalf("current observation lost: %v", got.Lifecycle)
	}
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{UID: "group-u"}}
	if labGroupToProto(g).Uid != "group-u" {
		t.Fatal("group UID lost")
	}
}

func TestLifecycleCRDModelValidation(t *testing.T) {
	h, k8s := newTestHandler(t)
	mustNamespace(t, k8s, "lifecycle-model")
	ctx := context.Background()
	labs := h.cs.LaboratoryV1alpha1().Labs("lifecycle-model")
	for i, tc := range []struct {
		name   string
		intent lab.LabLifecycleSpec
	}{
		{"invalid state", lab.LabLifecycleSpec{DesiredState: "Paused", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}},
		{"empty operation", lab.LabLifecycleSpec{DesiredState: "Stopped", Revision: 1, SnapshotMode: "Skip"}},
		{"zero revision", lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", SnapshotMode: "Skip"}},
		{"missing policy", lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1}},
		{"invalid policy", lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Guess"}},
		{"terminal running", lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "op", Revision: 1, Terminal: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := labs.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "invalid-" + string(rune('a'+i)), Namespace: "lifecycle-model"}, Spec: lab.LabSpec{Lifecycle: &tc.intent}}, metav1.CreateOptions{})
			if !apierrors.IsInvalid(err) {
				t.Fatalf("invalid lifecycle accepted: %v", err)
			}
		})
	}
	for i, intent := range []*lab.LabLifecycleSpec{
		nil, {DesiredState: "Running", OperationID: "start", Revision: 1},
		{DesiredState: "Stopped", OperationID: "stop", Revision: 1, SnapshotMode: "Skip"},
		{DesiredState: "Stopped", OperationID: "stop", Revision: 1, SnapshotMode: "Required"},
	} {
		if _, err := labs.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "valid-" + string(rune('a'+i)), Namespace: "lifecycle-model"}, Spec: lab.LabSpec{Lifecycle: intent}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("valid lifecycle refused: %v", err)
		}
	}
	devices := h.cs.LaboratoryV1alpha1().Devices("lifecycle-model")
	create := func(name string, request *lab.DeviceCaptureRequest) (*lab.Device, error) {
		return devices.Create(ctx, &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lifecycle-model"}, Spec: lab.DeviceSpec{LabRef: "valid-a", Name: "web", Type: lab.DeviceTypeContainer, State: &lab.DeviceStateSpec{Enabled: true, CaptureRequest: request}}}, metav1.CreateOptions{})
	}
	for i, deadline := range []int32{0, 3601} {
		_, err := create("deadline-"+string(rune('a'+i)), &lab.DeviceCaptureRequest{OperationID: "capture", LifecycleRevision: 1, PodUID: "pod-u", PodResourceVersion: "5", DeadlineSeconds: deadline})
		if !apierrors.IsInvalid(err) {
			t.Fatalf("unbounded deadline %d accepted: %v", deadline, err)
		}
	}
	for i, deadline := range []int32{1, 3600} {
		if _, err := create("bounded-"+string(rune('a'+i)), &lab.DeviceCaptureRequest{OperationID: "capture", LifecycleRevision: 1, PodUID: "pod-u", PodResourceVersion: "5", DeadlineSeconds: deadline}); err != nil {
			t.Fatalf("bounded deadline %d refused: %v", deadline, err)
		}
	}
	device, err := create("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	device.Status.State = &lab.DeviceStateStatus{Image: "latest-valid", ExitSnapshotPod: "legacy-exit", Capture: &lab.DeviceCaptureResult{OperationID: "capture", LifecycleRevision: 1, PodUID: "pod-u", PodResourceVersion: "5", NodeAgentEpoch: "boot", Result: "Succeeded", GuardState: "Held", Quiesced: true}}
	device, err = devices.UpdateStatus(ctx, device, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Invalid capture enums cannot corrupt the independent old snapshot marker/image.
	bad := device.DeepCopy()
	bad.Status.State.Capture.Result = "Ignored"
	if _, err = devices.UpdateStatus(ctx, bad, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("ambiguous capture result accepted: %v", err)
	}
	got, err := devices.Get(ctx, device.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.State.ExitSnapshotPod != "legacy-exit" || got.Status.State.Image != "latest-valid" || got.Status.State.Capture.Result != "Succeeded" {
		t.Fatalf("legacy snapshot fields lost: %+v", got.Status.State)
	}
}

func TestLifecycleTerminalCannotRemoveSpec(t *testing.T) {
	h, k8s := newTestHandler(t)
	mustNamespace(t, k8s, "terminal-spec")
	ctx := context.Background()
	labs := h.cs.LaboratoryV1alpha1().Labs("terminal-spec")
	current, err := labs.Create(ctx, &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "solved", Namespace: "terminal-spec"}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "solve", Revision: 1, SnapshotMode: "Skip", Terminal: true}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Spec.Lifecycle = nil
	if _, err := labs.Update(ctx, current, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("removing terminal lifecycle reopened Lab: %v", err)
	}
	// JSON patch can remove the whole spec instead of only the nested lifecycle.
	if _, err := labs.Patch(ctx, current.Name, types.JSONPatchType, []byte(`[{"op":"remove","path":"/spec"}]`), metav1.PatchOptions{}); !apierrors.IsInvalid(err) {
		t.Fatalf("removing whole terminal spec reopened Lab: %v", err)
	}
}

func TestLifecycleAllocationProjectionKeepsSeparateAmounts(t *testing.T) {
	observed := metav1.Unix(100, 0)
	released := metav1.Unix(200, 0)
	allocation := &lab.RuntimeAllocation{ConfiguredRequests: lab.ResourceAmounts{CPUMillicores: 50, MemoryBytes: 80 << 20}, ConfiguredLimits: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 160 << 20}, AllocatedRequests: lab.ResourceAmounts{CPUMillicores: 50, MemoryBytes: 80 << 20}, RuntimeState: "Releasing", ObservedAt: &observed, ReleasedAt: &released, UsageAvailable: true, Used: &lab.ResourceAmounts{CPUMillicores: 12, MemoryBytes: 30 << 20}, SnapshotQuotaBytes: 1024, StorageState: "Retained", PhysicalStorageBytesAvailable: true, PhysicalStorageBytes: 512, OperationID: "stop", Revision: 4}
	for _, got := range []*protobuf.ResourceAllocation{labToProto(&lab.Lab{Status: lab.LabStatus{Resources: allocation}}).Status.Resources, labGroupToProto(&lab.LabGroup{Status: lab.LabGroupStatus{Resources: allocation}}).Status.Resources} {
		if got.ConfiguredRequests.CpuMillicores != 50 || got.ConfiguredLimits.MemoryBytes != 160<<20 || got.AllocatedRequests.MemoryBytes != 80<<20 || got.Used.MemoryBytes != 30<<20 || !got.UsageAvailable || got.SnapshotQuotaBytes != 1024 || got.PhysicalStorageBytes != 512 || !got.PhysicalStorageBytesAvailable || got.RuntimeState != "Releasing" || got.StorageState != "Retained" || got.ObservedUnixMs != 100000 || got.ReleasedUnixMs != 200000 || got.OperationId != "stop" || got.LifecycleRevision != 4 {
			t.Fatalf("resource meanings conflated: %v", got)
		}
	}
	allocation.UsageAvailable = false
	if got := allocationToProto(allocation); got.Used != nil {
		t.Fatalf("unavailable measured usage emitted: %v", got)
	}
	if allocationToProto(nil) != nil {
		t.Fatal("missing observation invented free capacity")
	}
	unknown := allocationToProto(&lab.RuntimeAllocation{AllocatedRequests: lab.ResourceAmounts{CPUMillicores: 50, MemoryBytes: 80 << 20}})
	if unknown.RuntimeState != "Unknown" || unknown.StorageState != "Unknown" || unknown.AllocatedRequests.MemoryBytes != 80<<20 {
		t.Fatalf("unknown observation released held capacity: %v", unknown)
	}
}

func TestLifecycleMetadataGenerationStaysSeparate(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "lab-u", Generation: 12}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 3, SnapshotMode: "Skip"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{ObservedState: "Stopped", LabUID: "lab-u", OperationID: "stop", Revision: 3, ObservedGeneration: 11, AccessFenced: true}}}
	for _, got := range []*protobuf.Lab{labToProto(l), labMonitoringToProto(l, "group")} {
		if got.Generation != 12 || got.Status.Lifecycle.ObservedState != "Unknown" || got.Status.Lifecycle.AccessFenced {
			t.Fatalf("stale observed generation accepted as current metadata: %v", got)
		}
	}
	l.Status.Lifecycle.ObservedGeneration = 12
	got := labToProto(l)
	if got.Generation != 12 || got.Status.Lifecycle.ObservedGeneration != 12 || got.Status.Lifecycle.ObservedState != "Stopped" || !got.Status.Lifecycle.AccessFenced {
		t.Fatalf("current observation lost: %v", got)
	}
	group := labGroupToProto(&lab.LabGroup{ObjectMeta: metav1.ObjectMeta{UID: "group-u", Generation: 15}})
	if group.Generation != 15 || group.Uid != "group-u" {
		t.Fatalf("group live identity lost: %v", group)
	}
}
