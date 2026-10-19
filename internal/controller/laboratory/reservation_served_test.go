package laboratory

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// noSetupManager is a manager that only answers GetRESTMapper: any other call (a controller being registered) panics.
type noSetupManager struct {
	manager.Manager
	mapper meta.RESTMapper
}

func (m noSetupManager) GetRESTMapper() meta.RESTMapper { return m.mapper }

type failingMapper struct{ meta.RESTMapper }

func (failingMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("discovery failed")
}

// Without the Reservation CRD the operator starts without the ReservationReconciler: nothing is registered and setup does
// not fail. A failure of the lookup itself is an error; a served kind is seen as served.
func TestReservationReconcilerNeedsTheCRD(t *testing.T) {
	r := &ReservationReconciler{}
	served, err := r.SetupWithManagerIfServed(noSetupManager{mapper: meta.NewDefaultRESTMapper(nil)})
	if err != nil || served {
		t.Fatalf("without the CRD: served %v, err %v", served, err)
	}
	if _, err := r.SetupWithManagerIfServed(noSetupManager{mapper: failingMapper{}}); err == nil {
		t.Fatal("a failed lookup was taken for a missing CRD")
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(laboratoryv1alpha1.SchemeGroupVersion.WithKind("Reservation"), meta.RESTScopeRoot)
	if served, err := ReservationServed(mapper); err != nil || !served {
		t.Fatalf("with the CRD: served %v, err %v", served, err)
	}
}
