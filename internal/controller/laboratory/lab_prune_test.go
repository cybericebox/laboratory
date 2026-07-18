package laboratory

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func pruneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func TestPruneDevices(t *testing.T) {
	s := pruneScheme(t)
	dev := func(name string) *laboratoryv1alpha1.Device {
		return &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "team-alpha",
				Labels:    map[string]string{names.LabelLab: "lab1"},
			},
			Spec: laboratoryv1alpha1.DeviceSpec{LabRef: "lab1"},
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(dev("lab1-attacker"), dev("lab1-victim")).Build()
	r := &LabReconciler{Client: c, Scheme: s}

	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab1", Namespace: "team-alpha"}}
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "attacker", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}

	if err := r.pruneDevices(context.Background(), lab); err != nil {
		t.Fatalf("pruneDevices: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: "lab1-attacker", Namespace: "team-alpha"}, &laboratoryv1alpha1.Device{}); err != nil {
		t.Errorf("attacker (in spec) should remain: %v", err)
	}
	err := r.Get(context.Background(), types.NamespacedName{Name: "lab1-victim", Namespace: "team-alpha"}, &laboratoryv1alpha1.Device{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("victim (removed from spec) should be pruned, got err=%v", err)
	}
}

func TestPruneConnections(t *testing.T) {
	s := pruneScheme(t)
	epsA := []laboratoryv1alpha1.EndpointSpec{{Device: "attacker"}, {Device: "victim"}}
	epsB := []laboratoryv1alpha1.EndpointSpec{{Device: "attacker"}, {Device: "gw"}}
	nameA := connectionName("lab1", epsA)
	nameB := connectionName("lab1", epsB)

	conn := func(name string, eps []laboratoryv1alpha1.EndpointSpec) *laboratoryv1alpha1.Connection {
		return &laboratoryv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "team-alpha",
				Labels:    map[string]string{names.LabelLab: "lab1"},
			},
			Spec: laboratoryv1alpha1.ConnectionSpec{LabRef: "lab1", Endpoints: eps},
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(conn(nameA, epsA), conn(nameB, epsB)).Build()
	r := &LabReconciler{Client: c, Scheme: s}

	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab1", Namespace: "team-alpha"}}
	lab.Spec.Connections = []laboratoryv1alpha1.ConnectionTemplate{{Endpoints: epsA}}

	if err := r.pruneConnections(context.Background(), lab); err != nil {
		t.Fatalf("pruneConnections: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: nameA, Namespace: "team-alpha"}, &laboratoryv1alpha1.Connection{}); err != nil {
		t.Errorf("connection A (in spec) should remain: %v", err)
	}
	err := r.Get(context.Background(), types.NamespacedName{Name: nameB, Namespace: "team-alpha"}, &laboratoryv1alpha1.Connection{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("connection B (removed from spec) should be pruned, got err=%v", err)
	}
}
