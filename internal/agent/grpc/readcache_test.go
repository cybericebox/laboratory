package grpc

import (
	"context"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// The readiness poll of the backend asks for one lab per call. With the read cache the API server is asked for the group
// and the lab only while the informers sync, not once per call.
func TestReadCacheTakesTheReadCallsOffTheAPIServer(t *testing.T) {
	group := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Labels: map[string]string{names.LabelTenant: names.DefaultTenant}},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "g-ns"},
	}
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g-ns", Labels: map[string]string{names.LabelTenant: names.DefaultTenant}}}
	dev := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "a-web", Namespace: "g-ns"}, Spec: laboratoryv1alpha1.DeviceSpec{LabRef: "a", Name: "web"}}
	res1 := &laboratoryv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: "r1"}, Spec: laboratoryv1alpha1.ReservationSpec{Tenant: names.DefaultTenant}}
	run := func(cached bool) (reads int64) {
		cs := fake.NewSimpleClientset(group.DeepCopy(), lab.DeepCopy(), dev.DeepCopy(), res1.DeepCopy())
		h := NewHandler(cs, nil, nil)
		if cached {
			h.EnableReadCache()
			h.cached() // the informers' own first LIST and WATCH are not the per-call load
		}
		var n atomic.Int64
		cs.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) { n.Add(1); return false, nil, nil })
		cs.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) { n.Add(1); return false, nil, nil })
		for i := 0; i < 100; i++ {
			res, err := h.ListLabs(context.Background(), &protobuf.ListRequest{Items: []*protobuf.ItemRef{{LabGroup: "g", Name: "a"}}})
			if err != nil || len(res.Items) != 1 {
				t.Fatalf("call %d: %v %v", i, res, err)
			}
			if res, err := h.ListLabs(context.Background(), &protobuf.ListRequest{LabGroup: "g"}); err != nil || len(res.Items) != 1 {
				t.Fatalf("group call %d: %v %v", i, res, err)
			}
		}
		return n.Load()
	}
	direct, cached := run(false), run(true)
	t.Logf("API server reads for 200 ListLabs calls: direct %d, cached %d", direct, cached)
	if cached*10 > direct {
		t.Fatalf("the cache must take most reads off the API server: direct %d, cached %d", direct, cached)
	}
}

// A miss is never a verdict: a lab created after the cache synced is found by the API server fallback.
func TestReadCacheMissFallsBackToTheAPIServer(t *testing.T) {
	group := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Labels: map[string]string{names.LabelTenant: names.DefaultTenant}},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "g-ns"},
	}
	cs := fake.NewSimpleClientset(group)
	h := NewHandler(cs, nil, nil)
	h.EnableReadCache()
	h.cached()
	// Hide the object from the informers: a create the cache has not seen yet.
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "late", Namespace: "g-ns"}}
	if _, err := cs.LaboratoryV1alpha1().Labs("g-ns").Create(context.Background(), lab, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := h.readLab(context.Background(), "g-ns", "late")
	if err != nil || got.Name != "late" {
		t.Fatalf("%v %v", got, err)
	}
}
