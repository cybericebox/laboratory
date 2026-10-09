package laboratory

import (
	"context"
	"strings"
	"testing"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This is the durable state after the collective capture/deletion barrier:
// both original writers have committed snapshots and exact native absence ACKs.
// Barrier creation and deletion are covered by the SnapshotBarrier tests.
func committedReleasedFixture(t *testing.T) (*LabReconciler, *lab.Lab, []*lab.Device, client.Client) {
	t.Helper()
	r, l, d, c, p := requiredReadyFixture(t)
	ctx := context.Background()
	if err := allocation.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	r.RuntimeObservation = true
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{names.LabelNodeAgentReady: "true"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "kernel-boot", OperatingSystem: "linux"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	second := d.DeepCopy()
	second.Name, second.UID, second.Spec.Name, second.ResourceVersion = "a-db", "device-db", "db", ""
	second.Spec.State.CaptureRequest.PodUID = "pod-db"
	second.Status.State.Capture.PodUID = "pod-db"
	second.Status.RuntimeReports[0].Identity.PodUID = "pod-db"
	second.Status.RuntimeReports[0].Identity.ContainerIDs = []string{"container-pod-db"}
	second.Status.RuntimeReports[0].Identity.CgroupPaths = []string{"/owned/pod-db"}
	second.Status.RuntimeReports[0].Identity.PortKeys = []string{"owned-port-pod-db"}
	if err := c.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	devices := []*lab.Device{d, second}
	for _, device := range devices {
		identity := device.Status.RuntimeReports[0].Identity
		identity.Namespace, identity.LabName = l.Namespace, l.Name
		identity.ScopeUID, identity.Generation = string(device.UID), l.Generation
		identity.Requests = lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 64 << 20}
		identity.Limits = identity.Requests
		device.Status.RuntimeInventory = []lab.OwnedRuntimeIdentity{identity}
		device.Status.RuntimeReports = []lab.OwnedRuntimeReport{waveReleased(identity)}
		device.Status.State.SizeBytes = 165
		if err := c.Status().Update(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	l.Status.Lifecycle = &lab.LabLifecycleStatus{ObservedState: "Unknown", OperationID: l.Spec.Lifecycle.OperationID, Revision: l.Spec.Lifecycle.Revision, LabUID: string(l.UID), ObservedGeneration: l.Generation, SnapshotComplete: true}
	scope := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: string(l.UID), OwnerUID: string(l.UID), Namespace: l.Namespace, LabName: l.Name, OperationID: l.Spec.Lifecycle.OperationID, Revision: l.Spec.Lifecycle.Revision, Generation: l.Generation, NodeName: node.Name, NodeBootID: node.Status.NodeInfo.BootID, AttachmentsComplete: true}
	l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{scope}
	l.Status.ScopeReports = []lab.OwnedRuntimeReport{waveReleased(scope)}
	if err := c.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	return r, l, devices, c
}

func TestCommittedRequiredReleaseReconcilePersistsStopped(t *testing.T) {
	r, l, devices, c := committedReleasedFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle.ObservedState != "Stopped" || !l.Status.Lifecycle.SnapshotComplete || l.Status.Resources == nil || l.Status.Resources.RuntimeState != "Released" || l.Status.Resources.AllocatedRequests != (lab.ResourceAmounts{}) {
		t.Fatalf("committed native release did not persist Stopped/Released: lifecycle=%+v resources=%+v", l.Status.Lifecycle, l.Status.Resources)
	}
	if l.Status.Resources.SnapshotQuotaBytes != 330 || l.Status.Resources.StorageState != "Retained" {
		t.Fatal("native release discarded the saved snapshots")
	}
	for _, device := range devices {
		if err := c.Get(ctx, client.ObjectKeyFromObject(device), device); err != nil {
			t.Fatal(err)
		}
		if device.Status.State.Image != "snapshot" || !device.Status.State.Capture.Committed {
			t.Fatal("release changed the original committed capture")
		}
	}
}

func TestCommittedRequiredReleaseRejectsForeignOrUnsetBarrier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*lab.Lab)
	}{
		{"foreignUID", func(l *lab.Lab) { l.Status.Lifecycle.LabUID = "foreign-lab" }},
		{"oldOperation", func(l *lab.Lab) { l.Status.Lifecycle.OperationID = "old-stop" }},
		{"oldRevision", func(l *lab.Lab) { l.Status.Lifecycle.Revision-- }},
		{"oldGeneration", func(l *lab.Lab) { l.Status.Lifecycle.ObservedGeneration-- }},
		{"unsetBarrier", func(l *lab.Lab) { l.Status.Lifecycle.SnapshotComplete = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l, _, c := committedReleasedFixture(t)
			ctx := context.Background()
			tc.mutate(l)
			if err := c.Status().Update(ctx, l); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			if l.Status.Lifecycle.ObservedState != "StopFailed" || l.Status.Lifecycle.SnapshotComplete || l.Status.Lifecycle.Reason != "PreparationFailed" || !strings.Contains(l.Status.Lifecycle.Error, "no fresh native checkpoint handshake") {
				t.Fatalf("unaccepted stop inherited committed barrier: %+v", l.Status.Lifecycle)
			}
			if l.Status.Resources != nil && l.Status.Resources.RuntimeState == "Released" {
				t.Fatal("unaccepted stop credited native release")
			}
		})
	}
}

func TestCommittedRequiredReleaseDoesNotGrantNewStart(t *testing.T) {
	r, l, _, c := committedReleasedFixture(t)
	ctx := context.Background()
	l.Spec.Lifecycle.DesiredState = "Running"
	l.Spec.Lifecycle.OperationID = "new-start"
	l.Spec.Lifecycle.Revision++
	l.Generation++
	if err := c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle.ObservedState != "Starting" || l.Status.Lifecycle.SnapshotComplete || l.Status.Lifecycle.OperationID != "new-start" || l.Status.Resources == nil || l.Status.Resources.RuntimeState == "Released" {
		t.Fatalf("new start inherited completed stop: lifecycle=%+v resources=%+v", l.Status.Lifecycle, l.Status.Resources)
	}
}

func TestCommittedRequiredReleaseStillRequiresNativeAndAccessProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*lab.Lab, *lab.Device)
	}{
		{"missingRuntimeReport", func(_ *lab.Lab, d *lab.Device) { d.Status.RuntimeReports = nil }},
		{"unknownRuntime", func(_ *lab.Lab, d *lab.Device) { d.Status.RuntimeReports[0].RuntimeState = "Unknown" }},
		{"foreignRuntime", func(_ *lab.Lab, d *lab.Device) { d.Status.RuntimeReports[0].Identity.PodUID = "replacement-pod" }},
		{"missingAbsenceCertificate", func(_ *lab.Lab, d *lab.Device) { d.Status.RuntimeReports[0].CgroupAbsentAt = nil }},
		{"missingFabricReport", func(l *lab.Lab, _ *lab.Device) { l.Status.ScopeReports = nil }},
		{"unknownFabric", func(l *lab.Lab, _ *lab.Device) { l.Status.ScopeReports[0].RuntimeState = "Unknown" }},
		{"foreignFabric", func(l *lab.Lab, _ *lab.Device) { l.Status.ScopeReports[0].Identity.NodeBootID = "foreign-boot" }},
		{"missingAccessFence", func(l *lab.Lab, _ *lab.Device) { l.Spec.VPN.Enabled = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l, devices, c := committedReleasedFixture(t)
			ctx := context.Background()
			tc.mutate(l, devices[0])
			status := l.DeepCopy().Status
			if err := c.Update(ctx, l); err != nil {
				t.Fatal(err)
			}
			l.Status = status
			if err := c.Status().Update(ctx, l); err != nil {
				t.Fatal(err)
			}
			if err := c.Status().Update(ctx, devices[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			if l.Status.Lifecycle.ObservedState == "Stopped" || !l.Status.Lifecycle.SnapshotComplete || l.Status.Resources != nil && l.Status.Resources.RuntimeState == "Released" {
				t.Fatalf("missing native/access proof credited release: lifecycle=%+v resources=%+v", l.Status.Lifecycle, l.Status.Resources)
			}
			if tc.name == "missingAccessFence" {
				if l.Status.Lifecycle.AccessFenced || l.Status.Lifecycle.Reason != "WaitingForAccessFence" {
					t.Fatal("committed barrier skipped access fence")
				}
			} else if l.Status.Resources == nil || l.Status.Resources.RuntimeState != "Unknown" || l.Status.Resources.SnapshotQuotaBytes != 330 || l.Status.Resources.StorageState != "Retained" {
				t.Fatalf("unknown native release discarded retained snapshots: %+v", l.Status.Resources)
			}
			for _, device := range devices {
				if err := c.Get(ctx, client.ObjectKeyFromObject(device), device); err != nil {
					t.Fatal(err)
				}
				if !device.Status.State.Capture.Committed || device.Status.State.Image != "snapshot" {
					t.Fatal("blocked native release changed committed capture")
				}
			}
		})
	}
}
