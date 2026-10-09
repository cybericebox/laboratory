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
	"github.com/ovn-org/libovsdb/ovsdb"
	"google.golang.org/protobuf/types/known/anypb"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model the actual retained pre-materialization declaration after the original
// persistent writer's current Required capture and native release completed.
func persistentHistoricalNeverFixture(t *testing.T) (*NativeRuntimeObserver, lab.OwnedRuntimeIdentity, *lab.Lab, *lab.Device) {
	t.Helper()
	o, id, l, d := historicalNeverFixture(t)
	// The stopped writer no longer has a live owner journal; retained API
	// inventory must still bring its known physical debt into the native rescan.
	if err := os.Remove(o.recordPath("owner", d.Status.RuntimeReports[0].Identity)); err != nil {
		t.Fatal(err)
	}
	id.Generation, id.OperationID, id.Revision = 2, "required-primary-initial", 1
	l.Generation = 8
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", SnapshotMode: "Required", OperationID: "current7", Revision: 7}
	l.Status.Lifecycle = &lab.LabLifecycleStatus{LabUID: string(l.UID), OperationID: "current7", Revision: 7, ObservedGeneration: 8, SnapshotComplete: true, ObservedState: "Unknown"}
	l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{id}
	proof := &d.Status.RuntimeReports[0].Identity
	proof.Generation, proof.OperationID, proof.Revision, proof.Incarnation = 8, "current7", 7, 1
	d.Spec.State = &lab.DeviceStateSpec{Enabled: true}
	d.Status.State = &lab.DeviceStateStatus{Epoch: 0, Incarnation: 1, Image: "committed-snapshot", Capture: &lab.DeviceCaptureResult{OperationID: "current7", LifecycleRevision: 7, PodUID: proof.PodUID, Epoch: 0, Incarnation: 1, Result: "Succeeded", Quiesced: true, GuardState: "Held", Committed: true, Image: "committed-snapshot", NodeAgentEpoch: "node-agent-epoch"}}
	d.Status.RuntimeInventory = []lab.OwnedRuntimeIdentity{*proof.DeepCopy()}
	c := o.Reader.(client.Client)
	for _, object := range []client.Object{l, d} {
		if err := c.Update(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	return o, id, l, d
}

func TestPersistentHistoricalNeverMaterializedNativeRescanReleasesOriginalScope(t *testing.T) {
	o, id, _, _ := persistentHistoricalNeverFixture(t)
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Released" || report.Error != "" || !report.Identity.AttachmentsComplete || report.RuntimeAbsentAt == nil || report.CgroupAbsentAt == nil || report.AttachmentsAbsentAt == nil {
		t.Fatalf("persistent original historical scope failed native release: %+v", report)
	}
	if report.Identity.Generation != 2 || report.Identity.Revision != 1 || report.Identity.OperationID != "required-primary-initial" || report.Identity.ScopeUID != "device" || report.Identity.Epoch != 0 || report.Identity.Incarnation != 0 || report.Identity.PodUID != "" || len(report.Identity.ContainerIDs) != 1 || report.Identity.ContainerIDs[0] != "original-container" || len(report.Identity.CgroupPaths) != 1 {
		t.Fatalf("persistent historical declaration rebound or known debt lost: %+v", report.Identity)
	}
	var durable lab.OwnedRuntimeReport
	if err := o.readRecord("scope-fabric-released", id, &durable); err != nil || !committedRuntimeReport(durable, report.Identity) {
		t.Fatalf("native persistent release was not committed: %v %+v", err, durable)
	}
}

func TestPersistentHistoricalNeverMaterializedRejectsUnboundProof(t *testing.T) {
	changes := map[string]func(*lab.OwnedRuntimeIdentity, *lab.Lab, *lab.Device){
		"replacement-device": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.UID = "replacement" },
		"replacement-lab":    func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.UID = "replacement" },
		"device-owner":       func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.OwnerReferences[0].UID = "foreign" },
		"device-lab-ref":     func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Spec.LabRef = "foreign" },
		"device-node":        func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.NodeName = "foreign" },
		"device-epoch":       func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Epoch++ },
		"device-incarnation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Incarnation++ },
		"coherent-changed-epoch": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Epoch++
			d.Status.State.Capture.Epoch = d.Status.State.Epoch
			d.Status.RuntimeInventory[0].Epoch = d.Status.State.Epoch
			d.Status.RuntimeReports[0].Identity.Epoch = d.Status.State.Epoch
		},
		"coherent-changed-incarnation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Incarnation++
			d.Status.State.Capture.Incarnation = d.Status.State.Incarnation
			d.Status.RuntimeInventory[0].Incarnation = d.Status.State.Incarnation
			d.Status.RuntimeReports[0].Identity.Incarnation = d.Status.State.Incarnation
		},
		"missing-state":        func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State = nil },
		"disabled-persistence": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Spec.State.Enabled = false },
		"not-retained":         func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Status.ScopeInventory = nil },
		"positive-old-declaration": func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			l.Status.ScopeInventory[0].ContainerIDs = []string{"old-debt"}
		},
		"current-declaration": func(id *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			id.Generation, id.OperationID, id.Revision = 8, "current7", 7
			l.Status.ScopeInventory[0] = *id
		},
		"new-start": func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			l.Spec.Lifecycle.DesiredState = "Running"
		},
		"skip-stop":       func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Spec.Lifecycle.SnapshotMode = "Skip" },
		"missing-barrier": func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Status.Lifecycle = nil },
		"unset-barrier": func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			l.Status.Lifecycle.SnapshotComplete = false
		},
		"barrier-lab": func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Status.Lifecycle.LabUID = "foreign" },
		"barrier-operation": func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) {
			l.Status.Lifecycle.OperationID = "previous"
		},
		"barrier-revision":    func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Status.Lifecycle.Revision-- },
		"barrier-generation":  func(_ *lab.OwnedRuntimeIdentity, l *lab.Lab, _ *lab.Device) { l.Status.Lifecycle.ObservedGeneration-- },
		"missing-capture":     func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Capture = nil },
		"uncommitted-capture": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Capture.Committed = false },
		"invalidated-capture": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Capture.GuardState = "Invalidated"
		},
		"failed-capture":     func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Capture.Result = "Failed" },
		"unquiesced-capture": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Capture.Quiesced = false },
		"capture-operation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Capture.OperationID = "previous"
		},
		"capture-revision": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Capture.LifecycleRevision--
		},
		"capture-pod": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Capture.PodUID = "replacement"
		},
		"capture-epoch":       func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Capture.Epoch++ },
		"capture-incarnation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.State.Capture.Incarnation++ },
		"capture-native-epoch": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.State.Capture.NodeAgentEpoch = ""
		},
		"missing-inventory": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.RuntimeInventory = nil },
		"inventory-pod": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].PodUID = "replacement"
		},
		"inventory-device": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].ScopeUID = "foreign"
		},
		"inventory-owner": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].OwnerUID = "foreign"
		},
		"inventory-node-boot": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].NodeBootID = "foreign"
		},
		"inventory-operation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].OperationID = "previous"
		},
		"inventory-revision": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.RuntimeInventory[0].Revision-- },
		"inventory-generation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].Generation--
		},
		"inventory-epoch": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.RuntimeInventory[0].Epoch++ },
		"inventory-incarnation": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].Incarnation++
		},
		"inventory-native-debt": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].CgroupPaths = nil
		},
		"inventory-attachments": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeInventory[0].AttachmentsComplete = false
		},
		"missing-report": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) { d.Status.RuntimeReports = nil },
		"report-mismatch": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Identity.ContainerIDs = []string{"replacement"}
		},
		"unknown-report": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].RuntimeState = "Unknown"
		},
		"report-error": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].Error = "unattributed"
		},
		"missing-observed-stamp": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].ObservedAt = nil
		},
		"missing-runtime-stamp": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].RuntimeAbsentAt = nil
		},
		"missing-cgroup-stamp": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].CgroupAbsentAt = nil
		},
		"missing-attachment-stamp": func(_ *lab.OwnedRuntimeIdentity, _ *lab.Lab, d *lab.Device) {
			d.Status.RuntimeReports[0].AttachmentsAbsentAt = nil
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			o, id, l, d := persistentHistoricalNeverFixture(t)
			change(&id, l, d)
			c := o.Reader.(client.Client)
			for _, object := range []client.Object{l, d} {
				if err := c.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			report := o.ObserveScope(context.Background(), id)
			if report.RuntimeState != "Unknown" || report.RuntimeAbsentAt != nil || report.AttachmentsAbsentAt != nil {
				t.Fatalf("unbound persistent history minted absence: %+v", report)
			}
		})
	}
}

func TestPersistentHistoricalNeverMaterializedKnownDebtStillRequiresNativeAbsence(t *testing.T) {
	for _, scenario := range []string{"populated-known-cgroup", "live-original-task", "live-replacement-task", "reused-port-row"} {
		t.Run(scenario, func(t *testing.T) {
			o, id, _, d := persistentHistoricalNeverFixture(t)
			ctx := context.Background()
			proof := d.Status.RuntimeInventory[0]
			switch scenario {
			case "populated-known-cgroup":
				if err := os.WriteFile(filepath.Join(proof.CgroupPaths[0], "cgroup.events"), []byte("populated 1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "live-original-task", "live-replacement-task":
				cid, pod, path := "original-container", proof.PodUID, "original-owned"
				if scenario == "live-replacement-task" {
					cid, pod, path = "replacement-container", "replacement-pod", "replacement-owned"
				}
				raw, err := json.Marshal(specs.Spec{Process: &specs.Process{Env: []string{"LIFECYCLE_LAB_UID=lab", "LIFECYCLE_DEVICE_UID=device"}}, Linux: &specs.Linux{CgroupsPath: path}})
				if err != nil {
					t.Fatal(err)
				}
				o.Runtime = scopeRuntimeAdapter(t, []*containers.Container{{ID: cid, Labels: map[string]string{"io.kubernetes.pod.uid": pod, "io.kubernetes.pod.namespace": "ns"}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: raw}}}, &task.Process{ID: cid, Pid: 42, Status: task.Status_RUNNING})
			case "reused-port-row":
				key := "p12345678"
				proof.PortKeys = []string{key}
				proof.PortRows = []lab.OwnedFabricPort{{Key: key, OwnerUID: proof.PodUID, RowUUID: "original-row"}}
				d.Status.RuntimeInventory[0], d.Status.RuntimeReports[0].Identity = proof, proof
				if err := o.Reader.(client.Client).Update(ctx, d); err != nil {
					t.Fatal(err)
				}
				ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key, portOwnerExternalID: "replacement-pod"})
				o.Network.OVS.client = scopeOVSAdapter{rows: []ovsdb.Row{{"_uuid": ovsdb.UUID{GoUUID: "replacement-row"}, "name": key, "external_ids": ids}}}
			}
			report := o.ObserveScope(ctx, id)
			if report.RuntimeState == "Released" || report.AttachmentsAbsentAt != nil {
				t.Fatalf("positive history bypassed native rescan: %+v", report)
			}
			if scenario == "populated-known-cgroup" && (report.Error != "scope cgroup remains populated" || len(report.Identity.CgroupPaths) != 1) {
				t.Fatalf("journal absence discarded known physical debt: %+v", report)
			}
			var durable lab.OwnedRuntimeReport
			if err := o.readRecord("scope-fabric-released", id, &durable); err == nil {
				t.Fatal("native remnant committed release")
			}
		})
	}
}

func TestPersistentHistoricalNeverMaterializedChangedBarrierBeforeFinalFenceStaysUnknown(t *testing.T) {
	o, id, l, _ := persistentHistoricalNeverFixture(t)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		l.Status.Lifecycle.SnapshotComplete = false
		return nil, o.Reader.(client.Client).Update(context.Background(), l)
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.Error != ErrPortOwnerChanged.Error() || report.AttachmentsAbsentAt != nil {
		t.Fatalf("changed barrier bypassed final native fence: %+v", report)
	}
}
