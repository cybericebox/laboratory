//go:build linux

package nodeagent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	containers "github.com/containerd/containerd/api/services/containers/v1"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	task "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent/ofclient"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ovsclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These are explicitly native API adapters, not actual application/native
// qualification. They exercise the producer, never a seeded Released status.
type scopeOCIAdapter struct {
	containers.UnimplementedContainersServer
	items []*containers.Container
}

func (s *scopeOCIAdapter) List(context.Context, *containers.ListContainersRequest) (*containers.ListContainersResponse, error) {
	return &containers.ListContainersResponse{Containers: s.items}, nil
}
func (s *scopeOCIAdapter) Get(_ context.Context, r *containers.GetContainerRequest) (*containers.GetContainerResponse, error) {
	for _, item := range s.items {
		if item.ID == r.ID {
			return &containers.GetContainerResponse{Container: item}, nil
		}
	}
	return nil, fmt.Errorf("native metadata unavailable")
}

type scopeTaskAdapter struct {
	tasks.UnimplementedTasksServer
	items []*task.Process
}

func (s *scopeTaskAdapter) List(context.Context, *tasks.ListTasksRequest) (*tasks.ListTasksResponse, error) {
	return &tasks.ListTasksResponse{Tasks: s.items}, nil
}

type scopeOVSAdapter struct {
	ovsclient.Client
	rows []ovsdb.Row
}

func (s scopeOVSAdapter) Echo(context.Context) error { return nil }
func (s scopeOVSAdapter) Transact(_ context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	out := make([]ovsdb.OperationResult, len(ops))
	for n := range out {
		out[n].Rows = s.rows
	}
	return out, nil
}
func scopeRuntimeAdapter(t *testing.T, items []*containers.Container, processes ...*task.Process) *containerd.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "cice-oci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runtime.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	containers.RegisterContainersServer(server, &scopeOCIAdapter{items: items})
	tasks.RegisterTasksServer(server, &scopeTaskAdapter{items: processes})
	go server.Serve(ln)
	t.Cleanup(server.Stop)
	c, err := containerd.New(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func scopeFlowAdapter(t *testing.T) *ofclient.Client {
	return scopeFlowAdapterWith(t, func() map[string]uint32 { return nil }, nil)
}
func scopeFlowAdapterWith(t *testing.T, ports func() map[string]uint32, barrier func()) *ofclient.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "cice-of-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "flow.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			header := make([]byte, 8)
			if _, err := io.ReadFull(conn, header); err != nil {
				return
			}
			size := int(binary.BigEndian.Uint16(header[2:4]))
			body := make([]byte, size-8)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			switch header[1] {
			case 0:
				_, _ = conn.Write(header)
			case 18:
				current := ports()
				reply := make([]byte, 16+64*len(current))
				copy(reply, header)
				reply[1] = 19
				binary.BigEndian.PutUint16(reply[2:4], uint16(len(reply)))
				copy(reply[8:10], body[:2])
				offset := 16
				for name, no := range current {
					binary.BigEndian.PutUint32(reply[offset:offset+4], no)
					copy(reply[offset+16:offset+32], name)
					offset += 64
				}
				_, _ = conn.Write(reply)
			case 20:
				if barrier != nil {
					barrier()
				}
				header[1] = 21
				_, _ = conn.Write(header)
			}
		}
	}()
	c, err := ofclient.Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func scopeProducerAdapter(t *testing.T, items []*containers.Container, rows []ovsdb.Row) (*NativeRuntimeObserver, lab.OwnedRuntimeIdentity) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: "group", Generation: 2}, Spec: lab.LabGroupSpec{Lifecycle: &lab.GroupLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1}}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&corev1.Pod{}, "spec.nodeName", func(object client.Object) []string { return []string{object.(*corev1.Pod).Spec.NodeName} }).WithObjects(g).Build()
	ovs := &OVSManager{client: scopeOVSAdapter{rows: rows}}
	flow := &FlowManager{client: scopeFlowAdapter(t), nativeFlowRead: func(context.Context) ([]nativeFlow, error) { return nil, nil }}
	o := &NativeRuntimeObserver{Runtime: scopeRuntimeAdapter(t, items), Reader: reader, Namespace: "k8s.io", NodeName: "node", BootID: "boot", JournalDir: t.TempDir(), CgroupRoot: t.TempDir(), Network: &NetworkAttachReconciler{OVS: ovs, Flows: flow}}
	id := lab.OwnedRuntimeIdentity{ScopeKind: "GroupScope", ScopeUID: "group", OwnerUID: "group", OperationID: "stop", Revision: 1, Generation: 2, Namespace: "ns", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{}, CgroupPaths: []string{}, PortKeys: []string{}}
	return o, id
}
func TestResidualScopeMissingAPIPodAndOwnedOVSRemnantIsUnknown(t *testing.T) {
	ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: "p12345678", portOwnerExternalID: "gone-pod"})
	o, id := scopeProducerAdapter(t, nil, []ovsdb.Row{{"external_ids": ids, "_uuid": ovsdb.UUID{GoUUID: "row"}, "name": "p12345678"}})
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("API absence erased native attachment: %+v", report)
	}
}
func TestResidualScopeMetadataDisappearanceRetainsPopulatedOriginalCgroup(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	path := filepath.Join(o.CgroupRoot, "owned-original")
	_ = os.MkdirAll(path, 0700)
	_ = os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 1\n"), 0600)
	observed := *id.DeepCopy()
	observed.ContainerIDs = []string{"actual-seen-container"}
	observed.CgroupPaths = []string{path}
	if err := o.writeRecord("scope", id, observed); err != nil {
		t.Fatal(err)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.CgroupAbsentAt != nil || len(report.Identity.CgroupPaths) == 0 {
		t.Fatalf("metadata disappearance freed original cgroup: %+v", report)
	}
}
func TestResidualScopeRetainedSandboxUsesPositiveJournalAfterAPIPodGone(t *testing.T) {
	spec, _ := json.Marshal(specs.Spec{Linux: &specs.Linux{CgroupsPath: "owned"}})
	item := &containers.Container{ID: "retained-sandbox", Labels: map[string]string{"io.kubernetes.pod.uid": "original-pod", "io.kubernetes.pod.namespace": "ns", "io.kubernetes.pod.name": "gone"}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: spec}}
	o, id := scopeProducerAdapter(t, []*containers.Container{item}, nil)
	path := filepath.Join(o.CgroupRoot, "owned")
	_ = os.MkdirAll(path, 0700)
	_ = os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 0\n"), 0600)
	owner := lab.OwnedRuntimeIdentity{OwnerUID: "group", PodUID: "original-pod", OperationID: "running", Revision: 1, Generation: 1, Namespace: "ns", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"retained-sandbox"}, CgroupPaths: []string{path}, PortKeys: []string{}}
	if err := o.recordObligation(owner); err != nil {
		t.Fatal(err)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Released" || report.Error != "" || report.CgroupAbsentAt == nil || report.AttachmentsAbsentAt == nil {
		t.Fatalf("exact retained sandbox ownership lost: %+v", report)
	}
}
func TestResidualTrulyEmptyGroupScopeRequiresNativeEnumerationBarrier(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Released" || report.Error != "" || !report.Identity.AttachmentsComplete {
		t.Fatalf("authoritative native empty scope failed: %+v", report)
	}
}

func TestResidualPrephysicalDeploymentCaptureBeforeFirstCurrentReport(t *testing.T) {
	c, p, d, l := residualDeploymentAncestry(t)
	ctx := context.Background()
	p.Spec.NodeName = "node"
	p.Labels = map[string]string{names.LabelLab: l.Name, names.LabelDevice: d.Spec.Name}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "skip", Revision: 2, SnapshotMode: "Skip"}
	if err := c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(specs.Spec{Linux: &specs.Linux{CgroupsPath: "owned"}})
	item := &containers.Container{ID: "actual-container", Labels: map[string]string{"io.kubernetes.pod.uid": string(p.UID), "io.kubernetes.pod.namespace": p.Namespace}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: raw}}
	ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: "p12345678", portOwnerExternalID: string(p.UID)})
	ovs := &OVSManager{client: scopeOVSAdapter{rows: []ovsdb.Row{{"external_ids": ids, "_uuid": ovsdb.UUID{GoUUID: "actual-row"}, "name": "p12345678"}}}}
	o := &NativeRuntimeObserver{Runtime: scopeRuntimeAdapter(t, []*containers.Container{item}), Reader: c, Namespace: "k8s.io", NodeName: "node", BootID: "boot", JournalDir: t.TempDir(), CgroupRoot: t.TempDir(), Network: &NetworkAttachReconciler{OVS: ovs}}
	_ = os.MkdirAll(filepath.Join(o.CgroupRoot, "owned"), 0700)
	_ = os.WriteFile(filepath.Join(o.CgroupRoot, "owned", "cgroup.events"), []byte("populated 1\n"), 0600)
	// No owner/current-operation report is seeded. This is the actual hook called
	// before the first flow/kernel/row mutation, using native API adapters.
	ovs.mu.Lock()
	err := o.prepareRuntimeBeforeRetirement("p12345678", p.UID, "actual-row")
	ovs.mu.Unlock()
	if err != nil {
		t.Fatalf("Deployment-backed Skip prephysical capture failed: %v", err)
	}
	var saved lab.OwnedRuntimeIdentity
	if err := o.readRecord("owner", lab.OwnedRuntimeIdentity{PodUID: string(p.UID), NodeName: "node", NodeBootID: "boot"}, &saved); err != nil || saved.OwnerUID != string(l.UID) || saved.ScopeUID != string(d.UID) || saved.OperationID != "skip" || len(saved.CgroupPaths) != 1 || len(saved.ContainerIDs) != 1 || len(saved.PortRows) != 1 {
		t.Fatalf("current native inventory not durable before cleanup: %+v %v", saved, err)
	}
}

func TestResidualScopeOrphanVNIOnlyFlowCannotMintEmptyProof(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return []nativeFlow{{VNI: 901, HasVNI: true, Raw: "table=6,metadata=0x385 actions=output:7"}}, nil
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("orphan VNI-only flow credited empty scope: %+v", report)
	}
}
func TestResidualNativeFlowParserDoesNotTreatOriginRegisterAsVNI(t *testing.T) {
	flows, err := parseNativeFlows("cookie=0x0, table=0, priority=100,in_port=7 actions=load:0x1->NXM_NX_REG0[],load:0x385->OXM_OF_METADATA[],resubmit(,6)\n cookie=0x0, table=6, metadata=0x385 actions=output:7")
	if err != nil || len(flows) != 2 || flows[0].VNI != 901 || flows[1].VNI != 901 || flows[0].InPort != 7 {
		t.Fatalf("ambiguous flow-domain parse: %+v %v", flows, err)
	}
}
func TestResidualPositiveScopeHistorySurvivesLaterFabricFailure(t *testing.T) {
	raw, _ := json.Marshal(specs.Spec{Process: &specs.Process{Env: []string{"GROUP_UID=group"}}, Linux: &specs.Linux{CgroupsPath: "seen-before-error"}})
	item := &containers.Container{ID: "observed-before-error", Labels: map[string]string{"io.kubernetes.pod.uid": "original", "io.kubernetes.pod.namespace": "ns"}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: raw}}
	o, id := scopeProducerAdapter(t, []*containers.Container{item}, nil)
	path := filepath.Join(o.CgroupRoot, "seen-before-error")
	_ = os.MkdirAll(path, 0700)
	_ = os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 1\n"), 0600)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) { return nil, fmt.Errorf("native flow inspection lost") }
	first := o.ObserveScope(context.Background(), id)
	if first.RuntimeState != "Unknown" {
		t.Fatal("failed inspection minted credit")
	}
	var persisted lab.OwnedRuntimeIdentity
	if err := o.readRecord("scope", id, &persisted); err != nil || len(persisted.CgroupPaths) != 1 || len(persisted.ContainerIDs) != 1 {
		t.Fatalf("positive inventory vanished on later failure: %+v %v", persisted, err)
	}
	o.Runtime = scopeRuntimeAdapter(t, nil)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) { return nil, nil }
	next := o.ObserveScope(context.Background(), id)
	if next.RuntimeState != "Unknown" || next.CgroupAbsentAt != nil {
		t.Fatalf("failed prior scan erased still-populated cgroup: %+v", next)
	}
}
func TestResidualScopeLostNodeCannotMintRelease(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	id.NodeBootID = "different-boot"
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("lost node credited release: %+v", report)
	}
}
func TestResidualScopeReusedOVSRowCannotRetireOriginal(t *testing.T) {
	key := "p12345678"
	ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key, portOwnerExternalID: "original-pod"})
	o, id := scopeProducerAdapter(t, nil, []ovsdb.Row{{"_uuid": ovsdb.UUID{GoUUID: "replacement-row"}, "name": key, "external_ids": ids}})
	owner := lab.OwnedRuntimeIdentity{OwnerUID: "group", PodUID: "original-pod", OperationID: "before", Revision: 1, Generation: 1, Namespace: "ns", NodeName: "node", NodeBootID: "boot", PortKeys: []string{key}, PortRows: []lab.OwnedFabricPort{{Key: key, OwnerUID: "original-pod", RowUUID: "original-row"}}}
	if err := o.recordObligation(owner); err != nil {
		t.Fatal(err)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("replacement native row accepted: %+v", report)
	}
}
func TestResidualScopeUnownedLegacyFabricIsUnknown(t *testing.T) {
	ids, _ := ovsdb.NewOvsMap(map[string]string{})
	o, id := scopeProducerAdapter(t, nil, []ovsdb.Row{{"_uuid": ovsdb.UUID{GoUUID: "legacy-row"}, "name": "pt1234567890", "external_ids": ids}})
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("unowned native patch row credited absent: %+v", report)
	}
}
func TestResidualScopeOrphanOutputCannotMintEmptyProof(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return []nativeFlow{{Raw: "table=6 actions=output:998"}}, nil
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("orphan output flow credited absent: %+v", report)
	}
}
func TestResidualFabricInventoryDoesNotSkipAlreadyCompleteDeclaration(t *testing.T) {
	r, c, _, _, db := residualFabricFixture(t, false)
	keys := []string{patchPortName("ns", "conn", "a"), patchPortName("ns", "conn", "b")}
	if err := r.OVS.AddPatchPairOwned(keys[0], keys[1], c.UID); err != nil {
		t.Fatal(err)
	}
	o := &NativeRuntimeObserver{Reader: r.Reader, Network: &NetworkAttachReconciler{OVS: r.OVS, Flows: r.Flows}, JournalDir: t.TempDir(), NodeName: "node", BootID: "boot"}
	id := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", OwnerUID: "lab", Namespace: "ns", AttachmentsComplete: true}
	if err := o.captureScopeFabric(context.Background(), &id); err != nil || len(id.FabricPorts) != len(db.ports) || len(id.FabricPorts) != 2 {
		t.Fatalf("completed declaration skipped actual patch inventory: %+v %v", id, err)
	}
}
func TestResidualPartialNativeVNIMaskCannotBecomeExactDomain(t *testing.T) {
	if _, err := parseNativeFlows("table=6,metadata=0x385/0xff actions=output:7"); err == nil {
		t.Fatal("partial VNI mask accepted as exact owner domain")
	}
}
func TestResidualScopeMappedButUnownedIngressCannotMintEmptyProof(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	o.Network.Flows.client = scopeFlowAdapterWith(t, func() map[string]uint32 { return map[string]uint32{"foreign-unowned": 7} }, nil)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return []nativeFlow{{InPort: 7, Raw: "table=0,in_port=7 actions=drop"}}, nil
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("descriptor connectivity became owner proof: %+v", report)
	}
}
func TestResidualHistoricalScopePositiveInventoryStillReleasesAfterStart(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	ctx := context.Background()
	path := filepath.Join(o.CgroupRoot, "previous-owned")
	_ = os.MkdirAll(path, 0700)
	_ = os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 0\n"), 0600)
	seen := *id.DeepCopy()
	seen.ContainerIDs = []string{"previous-owned"}
	seen.CgroupPaths = []string{path}
	if err := o.writeRecord("scope", id, seen); err != nil {
		t.Fatal(err)
	}
	var group lab.LabGroup
	c := o.Reader.(client.Client)
	if err := c.Get(ctx, client.ObjectKey{Name: "g"}, &group); err != nil {
		t.Fatal(err)
	}
	group.Spec.Lifecycle = &lab.GroupLifecycleSpec{DesiredState: "Running", OperationID: "new-start", Revision: 2}
	group.Generation = 3
	group.Status.ServiceRuntime = []lab.OwnedRuntimeIdentity{id}
	if err := c.Update(ctx, &group); err != nil {
		t.Fatal(err)
	}
	report := o.ObserveScope(ctx, id)
	if report.RuntimeState != "Released" || report.Error != "" || len(report.Identity.CgroupPaths) != 1 {
		t.Fatalf("historical enrichment stranded exact prior obligation: %+v", report)
	}
}
func TestResidualHistoricalScopeRejectsWrongStableFieldAndCorruptedJournal(t *testing.T) {
	for _, scenario := range []string{"namespace", "lab-name", "operation", "generation", "owner", "node", "boot", "corrupted-journal"} {
		t.Run(scenario, func(t *testing.T) {
			o, id := scopeProducerAdapter(t, nil, nil)
			ctx := context.Background()
			c := o.Reader.(client.Client)
			var group lab.LabGroup
			_ = c.Get(ctx, client.ObjectKey{Name: "g"}, &group)
			group.Spec.Lifecycle = &lab.GroupLifecycleSpec{DesiredState: "Running", OperationID: "start", Revision: 2}
			group.Generation = 3
			group.Status.ServiceRuntime = []lab.OwnedRuntimeIdentity{id}
			if err := c.Update(ctx, &group); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "namespace":
				id.Namespace = "foreign"
			case "lab-name":
				id.LabName = "foreign"
			case "operation":
				id.OperationID = "foreign"
			case "generation":
				id.Generation++
			case "owner":
				id.OwnerUID = "foreign"
			case "node":
				id.NodeName = "foreign"
			case "boot":
				id.NodeBootID = "foreign"
			case "corrupted-journal":
				if err := o.writeRecord("scope", id, "corrupted-identity"); err != nil {
					t.Fatal(err)
				}
			}
			report := o.ObserveScope(ctx, id)
			if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
				t.Fatalf("invalid scope history minted native release: %+v", report)
			}
		})
	}
}
