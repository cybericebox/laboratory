//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	containers "github.com/containerd/containerd/api/services/containers/v1"
	task "github.com/containerd/containerd/api/types/task"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Producer adapters exercise native observation, not seeded scope release.
func historicalNeverFixture(t *testing.T) (*NativeRuntimeObserver, lab.OwnedRuntimeIdentity, *lab.Lab, *lab.Device) {
	t.Helper()
	o, _ := scopeProducerAdapter(t, nil, nil)
	id := lab.OwnedRuntimeIdentity{ScopeKind: "NeverMaterialized", ScopeUID: "device", OwnerUID: "lab", Namespace: "ns", LabName: "l", OperationID: "initial", Revision: 1, Generation: 1, NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{}, CgroupPaths: []string{}, PortKeys: []string{}}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 2}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Skip"}}, Status: lab.LabStatus{ScopeInventory: []lab.OwnedRuntimeIdentity{id}}}
	path := filepath.Join(o.CgroupRoot, "original-owned")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	proof := lab.OwnedRuntimeIdentity{ScopeUID: "device", OwnerUID: "lab", Namespace: "ns", LabName: "l", OperationID: "stop", Revision: 2, Generation: 2, PodUID: "original-pod", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"original-container"}, CgroupPaths: []string{path}, PortKeys: []string{}, AttachmentsComplete: true}
	now := metav1.Now()
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", UID: "device", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: "lab"}}}, Spec: lab.DeviceSpec{LabRef: "l", Name: "d", Type: lab.DeviceTypeContainer}, Status: lab.DeviceStatus{PodName: "original", NodeName: "node", RuntimeReports: []lab.OwnedRuntimeReport{{Identity: proof, RuntimeState: "Released", ObservedAt: &now, RuntimeAbsentAt: &now, CgroupAbsentAt: &now, AttachmentsAbsentAt: &now}}}}
	c := o.Reader.(client.Client)
	for _, object := range []client.Object{l, d} {
		if err := c.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.writeRecord("owner", proof, proof); err != nil {
		t.Fatal(err)
	}
	return o, id, l, d
}

func TestHistoricalNeverMaterializedNativeAbsenceReleasesOriginalScope(t *testing.T) {
	o, id, _, _ := historicalNeverFixture(t)
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Released" || report.Error != "" || !report.Identity.AttachmentsComplete || report.RuntimeAbsentAt == nil || report.CgroupAbsentAt == nil || report.AttachmentsAbsentAt == nil {
		t.Fatalf("original historical scope did not reach native release: %+v", report)
	}
	if report.Identity.Generation != 1 || report.Identity.Revision != 1 || report.Identity.OperationID != "initial" || report.Identity.ScopeUID != "device" || report.Identity.PodUID != "" || len(report.Identity.ContainerIDs) != 1 || report.Identity.ContainerIDs[0] != "original-container" || len(report.Identity.CgroupPaths) != 1 {
		t.Fatalf("old obligation rebound or positive owner history lost: %+v", report.Identity)
	}
	var durable lab.OwnedRuntimeReport
	if err := o.readRecord("scope-fabric-released", id, &durable); err != nil || !committedRuntimeReport(durable, report.Identity) {
		t.Fatalf("native release was not committed: %v %+v", err, durable)
	}
}

func TestHistoricalNeverMaterializedScopeGuardRejectsUnboundProof(t *testing.T) {
	changes := map[string]func(*lab.OwnedRuntimeIdentity, *lab.Lab, *lab.Device){
		"replacement-device-uid": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.UID = "replacement" },
		"device-lab-owner": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.OwnerReferences[0].UID = "replacement-lab"
		},
		"device-lab-name": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Spec.LabRef = "other" },
		"not-retained":    func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Status.ScopeInventory = nil },
		"current-materialized-declaration": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.Generation, id.Revision, id.OperationID = 2, 2, "stop"
			l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{*id}
		},
		"positive-old-container": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.ContainerIDs = []string{"prior"}
			l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{*id}
		},
		"positive-old-cgroup": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.CgroupPaths = []string{"prior"}
			l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{*id}
		},
		"positive-old-port": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.PortKeys = []string{"p12345678"}
			l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{*id}
		},
		"positive-old-vni": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.VNIs = []uint{901}
			l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{*id}
		},
		"positive-old-binding": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.VNIBindings = []lab.OwnedVNI{{UID: "device", VNI: 901}}
			l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{*id}
		},
		"missing-proof": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.RuntimeReports = nil },
		"proof-device": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.ScopeUID = "other-device"
		},
		"proof-owner": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.OwnerUID = "other-lab"
		},
		"proof-namespace": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.Namespace = "other"
		},
		"proof-lab-name": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.LabName = "other"
		},
		"proof-node": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.NodeName = "other"
		},
		"proof-boot": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.NodeBootID = "previous"
		},
		"proof-generation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.Generation = 1
		},
		"proof-operation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.OperationID = "initial"
		},
		"proof-revision": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.Revision = 1
		},
		"proof-epoch": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.Epoch = 1
		},
		"proof-incarnation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.Incarnation = 1
		},
		"proof-pod": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.PodUID = ""
		},
		"proof-containers": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.ContainerIDs = nil
		},
		"proof-cgroups": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.CgroupPaths = nil
		},
		"proof-attachments": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.AttachmentsComplete = false
		},
		"proof-error": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Error = "unknown"
		},
		"proof-observation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].ObservedAt = nil
		},
		"proof-state": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].RuntimeState = "Unknown"
		},
		"proof-release-incomplete": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].CgroupAbsentAt = nil
		},
		"persistent-incarnation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State = &lab.DeviceStateStatus{Epoch: 1, Incarnation: 1}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			o, id, l, d := historicalNeverFixture(t)
			change(&id, l, d)
			c := o.Reader.(client.Client)
			for _, object := range []client.Object{l, d} {
				if err := c.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := o.scopeCurrent(context.Background(), id); err == nil {
				t.Fatal("unbound proof bypassed scope eligibility")
			}
			report := o.ObserveScope(context.Background(), id)
			if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil || report.RuntimeAbsentAt != nil {
				t.Fatalf("unbound proof minted absence: %+v", report)
			}
		})
	}
}

func TestHistoricalNeverMaterializedOwnedCgroupRemainsUnknown(t *testing.T) {
	o, id, _, d := historicalNeverFixture(t)
	path := d.Status.RuntimeReports[0].Identity.CgroupPaths[0]
	if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.scopeCurrent(context.Background(), id); err != nil {
		t.Fatal("valid original proof did not reach native remnant scan:", err)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.Error != "scope cgroup remains populated" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("populated native debt gained release: %+v", report)
	}
}

func TestHistoricalNeverMaterializedSavedPositiveHistoryIsPreserved(t *testing.T) {
	o, id, _, _ := historicalNeverFixture(t)
	path := filepath.Join(o.CgroupRoot, "extra-owned")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	saved := *id.DeepCopy()
	saved.ContainerIDs, saved.CgroupPaths = []string{"extra-container"}, []string{path}
	if err := o.writeRecord("scope", id, saved); err != nil {
		t.Fatal(err)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.Error != "scope cgroup remains populated" || len(report.Identity.ContainerIDs) != 2 || len(report.Identity.CgroupPaths) != 2 || report.AttachmentsAbsentAt != nil {
		t.Fatalf("positive saved scope debt was erased: %+v", report)
	}
}

func TestHistoricalNeverMaterializedRetainedDebtChangedBeforeFinalFenceStaysUnknown(t *testing.T) {
	o, id, l, _ := historicalNeverFixture(t)
	c := o.Reader.(client.Client)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		l.Status.ScopeInventory[0].ContainerIDs = []string{"additional-retained-debt"}
		return nil, c.Update(context.Background(), l)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.Error != ErrPortOwnerChanged.Error() || report.AttachmentsAbsentAt != nil {
		t.Fatalf("changed positive original declaration bypassed final fence: %+v", report)
	}
}

func TestHistoricalNeverMaterializedLiveCurrentTaskStaysAllocated(t *testing.T) {
	o, id, l, d := historicalNeverFixture(t)
	l.Spec.Lifecycle.DesiredState = "Running"
	d.Status.RuntimeReports[0].RuntimeState = "Allocated"
	c := o.Reader.(client.Client)
	for _, object := range []client.Object{l, d} {
		if err := c.Update(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(specs.Spec{Process: &specs.Process{Env: []string{"LIFECYCLE_LAB_UID=lab", "LIFECYCLE_DEVICE_UID=device"}}, Linux: &specs.Linux{CgroupsPath: "original-owned"}})
	if err != nil {
		t.Fatal(err)
	}
	o.Runtime = scopeRuntimeAdapter(t, []*containers.Container{{ID: "original-container", Labels: map[string]string{"io.kubernetes.pod.uid": "original-pod", "io.kubernetes.pod.namespace": "ns"}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: raw}}}, &task.Process{ID: "original-container", Pid: 42, Status: task.Status_RUNNING})
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Allocated" || report.Error != "" || report.RuntimeAbsentAt != nil || report.AttachmentsAbsentAt != nil {
		t.Fatalf("old declaration retired live current task: %+v", report)
	}
}
