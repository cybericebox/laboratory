package activity

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func fixture(t *testing.T) (*Hub, *time.Time, func(string) laboratoryv1alpha1.LabActivityReport) {
	t.Helper()
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&laboratoryv1alpha1.LabActivityReport{}).Build()
	now := time.Unix(1_800_000_000, 0)
	h := New(c, "vpn")
	h.Now = func() time.Time { return now }
	get := func(ns string) laboratoryv1alpha1.LabActivityReport {
		var r laboratoryv1alpha1.LabActivityReport
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "vpn"}, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	return h, &now, get
}

func ev(at time.Time) laboratoryv1alpha1.ActivityEvent {
	return laboratoryv1alpha1.ActivityEvent{AtMs: at.UnixMilli(), Kind: laboratoryv1alpha1.ActivityTouch, LabName: "web", Client: "p1", DstAddr: "10.8.1.5", BytesUp: 10}
}

func TestDisabledHubIsANoOp(t *testing.T) {
	var h *Hub
	h.Add("ns", ev(time.Now()))
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Run(context.Background(), time.Second)
}

func TestEventsGetIncreasingSeqAndLandInTheReport(t *testing.T) {
	h, now, get := fixture(t)
	for i := 0; i < 3; i++ {
		h.Add("g", ev(*now))
	}
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := get("g")
	if r.Spec.Writer != "vpn" || len(r.Status.Events) != 3 || r.Status.Events[0].Seq >= r.Status.Events[1].Seq || r.Status.Events[1].Seq >= r.Status.Events[2].Seq {
		t.Fatalf("report = %+v", r)
	}
	if r.Status.DroppedThrough != 0 {
		t.Fatalf("nothing was dropped: %d", r.Status.DroppedThrough)
	}
}

func TestRingIsBoundedByCountAndAgeAndRemembersWhatItDropped(t *testing.T) {
	h, now, get := fixture(t)
	h.MaxEvents = 5
	for i := 0; i < 8; i++ {
		h.Add("g", ev(*now))
	}
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := get("g")
	if len(r.Status.Events) != 5 || r.Status.DroppedThrough != r.Status.Events[0].Seq-1 {
		t.Fatalf("count bound: %d events, droppedThrough %d, first seq %d", len(r.Status.Events), r.Status.DroppedThrough, r.Status.Events[0].Seq)
	}
	last := r.Status.Events[4].Seq
	*now = now.Add(2 * time.Hour)
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = get("g")
	if len(r.Status.Events) != 0 || r.Status.DroppedThrough != last {
		t.Fatalf("age bound: %d events, droppedThrough %d, want %d", len(r.Status.Events), r.Status.DroppedThrough, last)
	}
}

func TestRestartKeepsTheUnpulledEventsAndNeverReusesASeq(t *testing.T) {
	h, now, get := fixture(t)
	h.Add("g", ev(*now))
	h.Add("g", ev(*now))
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := get("g").Status.Events
	// a new process of the same writer
	h2 := New(h.Client, "vpn")
	*now = now.Add(10 * time.Second)
	h2.Now = h.Now
	h2.Add("g", ev(*now))
	if err := h2.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := get("g").Status.Events
	if len(after) != 3 || after[0].Seq != before[0].Seq || after[1].Seq != before[1].Seq || after[2].Seq <= before[1].Seq {
		t.Fatalf("before %+v after %+v", before, after)
	}
}

func TestIdleHubDoesNotWriteAgain(t *testing.T) {
	h, now, get := fixture(t)
	h.Add("g", ev(*now))
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	v1 := get("g").ResourceVersion
	if err := h.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v2 := get("g").ResourceVersion; v1 != v2 {
		t.Fatalf("an idle flush wrote: %s -> %s", v1, v2)
	}
}

func TestReportNameIsADNSName(t *testing.T) {
	if got := ReportName("Proxy-ABC_def.1"); got != "proxy-abc-def-1" {
		t.Fatal(got)
	}
	_ = metav1.Now
}
