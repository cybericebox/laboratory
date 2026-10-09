package flowacct

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type orderedCounters struct {
	mu               sync.Mutex
	started, release chan struct{}
}

func (r *orderedCounters) ReadPairCounters(context.Context) (CounterSnapshot, error) {
	close(r.started)
	<-r.release
	return CounterSnapshot{At: t0, Rows: []PairCounters{sample(10)}}, nil
}
func (r *orderedCounters) WithPairCounters(ctx context.Context, fn func(CounterSnapshot) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, e := r.ReadPairCounters(ctx)
	if e != nil {
		return e
	}
	return fn(s)
}
func TestPollCannotFoldOlderSnapshotAfterRetirement(t *testing.T) {
	reader := &orderedCounters{started: make(chan struct{}), release: make(chan struct{})}
	c := newCollector(&fakeSource{})
	r := &Reporter{Collector: c, CounterReader: reader}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.Poll(context.Background(), t0); err != nil {
			t.Error(err)
		}
	}()
	<-reader.started
	retired := make(chan struct{})
	go func() {
		defer close(retired)
		reader.mu.Lock()
		defer reader.mu.Unlock()
		observe(t, c, sample(12), t0.Add(time.Second))
		c.ForgetBindings([]string{"binding"})
	}()
	select {
	case <-retired:
	case <-time.After(20 * time.Millisecond):
	}
	close(reader.release)
	<-done
	<-retired
	if n := c.Snapshot(t0).Ledger[0].PacketsOut; n != 12 {
		t.Fatalf("late sample recounted retirement: %d", n)
	}
}

type pauseReader struct {
	client.Reader
	first            atomic.Bool
	entered, release chan struct{}
}

func (r *pauseReader) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if r.first.CompareAndSwap(false, true) {
		close(r.entered)
		<-r.release
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
func TestPublicationCannotOverwriteFinalRetirement(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	obj := &lab.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Namespace: "g", Name: ReportName}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj).Build()
	reader := &pauseReader{Reader: store, entered: make(chan struct{}), release: make(chan struct{})}
	c := newCollector(&fakeSource{})
	observe(t, c, sample(10), t0)
	r := &Reporter{Reader: reader, Writer: store, Namespace: "g", Collector: c}
	published := make(chan struct{})
	go func() {
		defer close(published)
		if err := r.Publish(context.Background(), t0); err != nil {
			t.Error(err)
		}
	}()
	<-reader.entered
	retired := make(chan struct{})
	go func() {
		defer close(retired)
		if err := r.Retire(context.Background(), CounterSnapshot{At: t0.Add(time.Second), Rows: []PairCounters{sample(12)}}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-retired:
	case <-time.After(20 * time.Millisecond):
	}
	close(reader.release)
	<-published
	<-retired
	var got lab.LabTrafficReport
	_ = store.Get(context.Background(), client.ObjectKeyFromObject(obj), &got)
	if len(got.Status.Ledger) != 1 || got.Status.Ledger[0].PacketsOut != 12 {
		t.Fatalf("final totals overwritten %+v", got.Status)
	}
}
func TestLabOriginalRepliesDoNotAdvanceHistoricalStudentActivity(t *testing.T) {
	c := newCollector(&fakeSource{})
	observe(t, c, sample(1), t0)
	cur := sample(5)
	cur.LabInitiatedAttempts = 1
	observe(t, c, cur, t0.Add(time.Minute))
	if got := c.Snapshot(t0).Ledger[0].LastSeenMs; got != t0.UnixMilli() {
		t.Fatalf("lab reply created student activity %d", got)
	}
}
func TestShutdownClosesGateBeforeFinalRead(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.LabTrafficReport{}).Build()
	order := []string{}
	r := &Reporter{Reader: store, Writer: store, Namespace: "g", Collector: newCollector(&fakeSource{}), BeforeShutdown: func(context.Context) error { order = append(order, "closed"); return nil }}
	r.CounterReader = counterReadFunc(func(context.Context) (CounterSnapshot, error) {
		if len(order) == 0 {
			t.Error("final read while gate open")
		}
		order = append(order, "read")
		return CounterSnapshot{At: t0}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx, time.Hour, time.Hour, func(error) {})
}

type counterReadFunc func(context.Context) (CounterSnapshot, error)

func (f counterReadFunc) ReadPairCounters(ctx context.Context) (CounterSnapshot, error) {
	return f(ctx)
}

func TestOldEndpointMetadataCannotMoveToNewOwner(t *testing.T) {
	source := &fakeSource{flows: []Flow{flow(1, clientA, labIP, 80)}}
	source.flows[0].Mark = 0x80000100
	source.flows[0].Replied = true
	c := New(source, func() Topology {
		topology := topo()
		topology.Clients = map[netip.Addr]string{clientA: "p-new"}
		return topology
	}, "boot", time.Second)
	cp := sample(1)
	cp.Subject = "p-new"
	cp.ClientCIDR = "10.8.0.3/32"
	observe(t, c, cp, t0)
	if err := c.Poll(t0); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot(t0).Ledger[0].FirstRespondMs != 0 {
		t.Fatal("old assigned address metadata moved to new owner")
	}
}
