package activity

import (
	"context"
	goruntime "runtime"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// counting wraps the client of a test and counts every call that reaches the API server.
type counting struct {
	client.Client
	reads, writes int
}

func newCounting(t *testing.T) (*Hub, *counting, *time.Time) {
	t.Helper()
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := &counting{}
	c.Client = fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&laboratoryv1alpha1.LabActivityReport{}).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			c.reads++
			return cl.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			c.writes++
			return cl.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			c.writes++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	now := time.Unix(1_800_000_000, 0)
	h := New(c, "vpn")
	h.Now = func() time.Time { return now }
	return h, c, &now
}

// An idle writer, and a group nobody does anything in (a lab startup), cost the API server nothing: not a write, not even a read.
func TestIdleWriterMakesNoAPICalls(t *testing.T) {
	h, c, _ := newCounting(t)
	for i := 0; i < 100; i++ {
		if err := h.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c.reads != 0 || c.writes != 0 {
		t.Fatalf("an idle hub made %d reads and %d writes", c.reads, c.writes)
	}
}

// Events cost one write per flush, and a flush with nothing new costs none: at most one write per interval, and only for new events.
func TestFlushWritesOnlyWhatIsNew(t *testing.T) {
	h, c, now := newCounting(t)
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		h.Add("g", ev(*now))
	}
	if err := h.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	first := c.writes // create + status
	if first != 2 {
		t.Fatalf("the first flush made %d writes, want create + one status write", first)
	}
	for i := 0; i < 50; i++ {
		if err := h.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if c.writes != first {
		t.Fatalf("flushes with nothing new wrote %d more times", c.writes-first)
	}
	reads := c.reads
	h.Add("g", ev(*now))
	if err := h.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if c.writes != first+1 {
		t.Fatalf("one new event cost %d writes", c.writes-first)
	}
	_ = reads
	if FlushEvery != time.Minute {
		t.Fatalf("FlushEvery = %v", FlushEvery)
	}
}

// Disabled (nil hub): no goroutine, no map, no call. Run returns at once.
func TestDisabledHubStartsNothing(t *testing.T) {
	before := goruntime.NumGoroutine()
	var h *Hub
	done := make(chan struct{})
	go func() { h.Run(context.Background(), time.Millisecond); close(done) }()
	<-done
	h.Add("g", ev(time.Now()))
	if h.Events("g") != nil || h.Flush(context.Background()) != nil {
		t.Fatal("a disabled hub holds or writes something")
	}
	if after := goruntime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

// The switch is read from one variable and is off unless it says true.
func TestEnabledFollowsTheEnvironmentAndDefaultsToOff(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	if Enabled() {
		t.Fatal("on by default")
	}
	for _, v := range []string{"false", "0", "TRUE", "yes"} {
		t.Setenv(EnvEnabled, v)
		if Enabled() {
			t.Fatalf("%q must not enable it", v)
		}
	}
	t.Setenv(EnvEnabled, "true")
	if !Enabled() {
		t.Fatal("true must enable it")
	}
}
