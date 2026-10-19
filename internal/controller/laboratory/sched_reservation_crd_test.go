package laboratory

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Without the Reservation CRD (an upgrade that did not apply the CRDs) the scheduler runs as if there were no reservations: an
// unreserved lab starts, a reserved one waits with ReservationMissing, and the warning is logged once, again only after the CRD
// was seen back.
func TestMissingReservationCRDIsNoReservations(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	missing := true
	f.s.Client = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*laboratoryv1alpha1.ReservationList); ok && missing {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: laboratoryv1alpha1.SchemeGroupVersion.Group, Kind: "Reservation"}}
			}
			return c.List(ctx, list, opts...)
		},
	})
	warnings := 0
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
		if strings.Contains(args, "Reservation CRD is not installed") {
			warnings++
		}
	}, funcr.Options{}))
	tick := func() {
		t.Helper()
		f.now = f.now.Add(1e9)
		if err := f.s.tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	f.addLab("u", "", nil, "p1")
	f.addLab("r", "", nil, "p1")
	f.labUnder("r", "r1", "t1")
	tick()
	tick()
	f.wantStates("r/p1=Q u/p1=S")
	if st := f.labStatus("r"); st == nil || st.Reason != laboratoryv1alpha1.WaitReservationMissing {
		t.Fatalf("r status = %+v", st)
	}
	if warnings != 1 {
		t.Fatalf("%d warnings over two ticks, want 1", warnings)
	}
	// the CRD is back, then gone again: one more warning
	missing = false
	tick()
	missing = true
	tick()
	tick()
	if warnings != 2 {
		t.Fatalf("%d warnings, want 2", warnings)
	}
}
