package laboratory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// This proves certificate consumption through the real status API. Native
// producer provenance remains a separate Linux/runtime proof.
func TestEmptyCurrentScopeAPIRoundTripReachesStoppedAndReleased(t *testing.T) {
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir()}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "empty-current"}}); err != nil {
		t.Fatal(err)
	}
	node, _ := residualPlacementNodes()
	status := node.Status
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	node.Status = status
	if err := c.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"zero-device-required", "never-materialized", "zero-leg-hub"} {
		t.Run(scenario, func(t *testing.T) {
			l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: scenario, Namespace: "empty-current"}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Skip"}}}
			if scenario == "zero-device-required" {
				l.Spec.Lifecycle.SnapshotMode = "Required"
			} else {
				kind := lab.DeviceTypeContainer
				if scenario == "zero-leg-hub" {
					kind = lab.DeviceTypeHub
				}
				l.Spec.Devices = []lab.DeviceTemplate{{Name: "device", Type: kind, Image: "nginx:alpine"}}
			}
			if err := c.Create(ctx, l); err != nil {
				t.Fatal(err)
			}
			if len(l.Spec.Devices) != 0 {
				d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: scenario + "-device", Namespace: l.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: l.Name, UID: l.UID}}}, Spec: lab.DeviceSpec{LabRef: l.Name, Name: "device", Type: l.Spec.Devices[0].Type, Image: "nginx:alpine", Code: "fixed"}}
				if err := c.Create(ctx, d); err != nil {
					t.Fatal(err)
				}
				d.Status.Scheduling = &lab.PodSchedule{State: lab.PodQueued}
				if err := c.Status().Update(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			r := &LabReconciler{Client: c, Reader: c, Scheme: scheme, RuntimeObservation: true}
			if err := r.prepareLabScopes(ctx, l); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			wantScopes := 1
			if scenario == "never-materialized" {
				wantScopes++
			}
			if len(l.Status.ScopeInventory) != wantScopes {
				t.Fatalf("missing actual declaration: %+v", l.Status.ScopeInventory)
			}
			for _, scope := range l.Status.ScopeInventory {
				scope.AttachmentsComplete = true
				l.Status.ScopeReports = append(l.Status.ScopeReports, waveReleased(scope))
			}
			if err := c.Status().Update(ctx, l); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			for _, report := range l.Status.ScopeReports {
				if report.Identity.VNIBindings != nil {
					t.Fatal("optional empty bindings did not round-trip to nil")
				}
			}
			// Preparation repeats on every stop reconciliation, after API decoding.
			if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState != "Stopped" || l.Status.Resources == nil || l.Status.Resources.RuntimeState != "Released" || l.Status.Resources.AllocatedRequests != (lab.ResourceAmounts{}) {
				t.Fatalf("current empty API certificate did not stop/release: lifecycle=%+v resources=%+v", l.Status.Lifecycle, l.Status.Resources)
			}
			if !runtimeRowsReleased(l.Status.ScopeInventory, l.Status.ScopeReports, string(l.UID), "stop", 2) {
				t.Fatal("prepared certificate no longer matches exact full identity")
			}
		})
	}
}

func TestEmptyCurrentScopeRetainsExactPositiveBindingAndIdentityRequirements(t *testing.T) {
	binding := lab.OwnedVNI{PoolUID: "pool", LeaseGeneration: 7, OwnerUID: "lab", OperationID: "stop", Revision: 2, Generation: 3, Kind: "Device", Namespace: "ns", Name: "gone", UID: "device", VNI: 0}
	changes := map[string]func(*lab.OwnedRuntimeReport){
		"exact-vni-zero":     func(*lab.OwnedRuntimeReport) {},
		"missing-binding":    func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings = nil },
		"foreign-binding":    func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].UID = "foreign" },
		"pool-uid":           func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].PoolUID = "replacement" },
		"lease-generation":   func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].LeaseGeneration++ },
		"binding-owner":      func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].OwnerUID = "foreign" },
		"binding-operation":  func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].OperationID = "old" },
		"binding-revision":   func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].Revision-- },
		"binding-generation": func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].Generation-- },
		"binding-namespace":  func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].Namespace = "foreign" },
		"binding-name":       func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].Name = "foreign" },
		"binding-kind":       func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].Kind = "Connection" },
		"nonzero-vni":        func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings[0].VNI = 1 },
		"owner":              func(r *lab.OwnedRuntimeReport) { r.Identity.OwnerUID = "foreign" },
		"scope-uid":          func(r *lab.OwnedRuntimeReport) { r.Identity.ScopeUID = "foreign" },
		"namespace":          func(r *lab.OwnedRuntimeReport) { r.Identity.Namespace = "foreign" },
		"operation":          func(r *lab.OwnedRuntimeReport) { r.Identity.OperationID = "old" },
		"revision":           func(r *lab.OwnedRuntimeReport) { r.Identity.Revision-- },
		"generation":         func(r *lab.OwnedRuntimeReport) { r.Identity.Generation-- },
		"node":               func(r *lab.OwnedRuntimeReport) { r.Identity.NodeName = "foreign" },
		"boot":               func(r *lab.OwnedRuntimeReport) { r.Identity.NodeBootID = "old-boot" },
		"incomplete":         func(r *lab.OwnedRuntimeReport) { r.Identity.AttachmentsComplete = false },
		"observed-time":      func(r *lab.OwnedRuntimeReport) { r.ObservedAt = nil },
		"runtime-time":       func(r *lab.OwnedRuntimeReport) { r.RuntimeAbsentAt = nil },
		"cgroup-time":        func(r *lab.OwnedRuntimeReport) { r.CgroupAbsentAt = nil },
		"attachment-time":    func(r *lab.OwnedRuntimeReport) { r.AttachmentsAbsentAt = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			scheme := pruneScheme(t)
			_ = corev1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)
			node, _ := residualPlacementNodes()
			l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Skip"}}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(l).WithObjects(l, node).Build()
			r := &LabReconciler{Client: c, Reader: c, Scheme: scheme, RuntimeObservation: true}
			if err := r.prepareLabScopes(ctx, l); err != nil {
				t.Fatal(err)
			}
			l.Status.ScopeInventory[0].VNIBindings = []lab.OwnedVNI{binding}
			l.Status.ScopeInventory[0].VNIs = []uint{0}
			id := *l.Status.ScopeInventory[0].DeepCopy()
			id.AttachmentsComplete = true
			report := waveReleased(id)
			change(&report)
			l.Status.ScopeReports = []lab.OwnedRuntimeReport{report}
			// Exercise the same optional JSON shape as status API publication.
			raw, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, l); err != nil {
				t.Fatal(err)
			}
			if err := c.Status().Update(ctx, l); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			valid := name == "exact-vni-zero"
			if valid != (l.Status.Lifecycle.ObservedState == "Stopped") || valid != (l.Status.Resources.RuntimeState == "Released") {
				t.Fatalf("binding/identity validity changed: valid=%v lifecycle=%+v resources=%+v", valid, l.Status.Lifecycle, l.Status.Resources)
			}
			if len(l.Status.ScopeInventory) != 1 || !reflect.DeepEqual(l.Status.ScopeInventory[0].VNIBindings, []lab.OwnedVNI{binding}) {
				t.Fatalf("positive VNI zero ownership debt was dropped/rebound: %+v", l.Status.ScopeInventory)
			}
		})
	}
}
