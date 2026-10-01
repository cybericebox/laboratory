package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// staleLabs makes Get of a Lab answer from a copy taken when the object was first read, as
// an informer cache does for a moment after the status was written.
type staleLabs struct {
	client.Client
	first map[client.ObjectKey]*laboratoryv1alpha1.Lab
}

func (s *staleLabs) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	lab, ok := obj.(*laboratoryv1alpha1.Lab)
	if !ok {
		return s.Client.Get(ctx, key, obj, opts...)
	}
	if old := s.first[key]; old != nil {
		old.DeepCopyInto(lab)
		return nil
	}
	if err := s.Client.Get(ctx, key, lab, opts...); err != nil {
		return err
	}
	s.first[key] = lab.DeepCopy()
	return nil
}

// A stale cache after the status write of the modes must not decide the mode of the devices:
// every device of a lab created with the switch on is snapshot-backed.
func TestDevicesKeepTheModeOfTheLabWhenTheCacheIsStale(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	lab := codeLab("l1", "uid-1")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lab).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}, &laboratoryv1alpha1.Device{}).Build()
	r := &LabReconciler{
		Client: &staleLabs{Client: c, first: map[client.ObjectKey]*laboratoryv1alpha1.Lab{}},
		Scheme: s, BaseDomain: "labs.example.com",
		State: StatePolicy{Enabled: true, MaxLayers: 10, MaxSnapshotBytes: 1 << 29},
	}
	// The first pass adds the finalizer; later passes reach the devices.
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-alpha", Name: "l1"}})
		r.Client.(*staleLabs).first = map[client.ObjectKey]*laboratoryv1alpha1.Lab{}
	}
	devs := devicesOf(t, c)
	if len(devs) != 2 {
		t.Fatalf("want 2 devices, got %d", len(devs))
	}
	for name, d := range devs {
		if !d.Spec.StateEnabled() {
			t.Errorf("device %s was created without state persistence", name)
		}
	}
}
