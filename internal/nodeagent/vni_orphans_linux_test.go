//go:build linux

package nodeagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vniledger"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

type orphanFixture struct {
	r        *VNIOrphanReconciler
	c        client.Client
	barriers *atomic.Int32 // one per RetireVNI that reached the switch
}

func orphanTest(t *testing.T, state string, flowVNIs []uint64, ledger ...uint) *orphanFixture {
	t.Helper()
	s := runtime.NewScheme()
	_ = lab.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocation.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocation.PoolSpec{Size: names.VNIPoolSize},
		Status:     allocation.PoolStatus{BitMap: bitmap, Free: free, Leases: map[string]allocation.PoolLease{}, Retired: map[string]allocation.PoolRetired{}},
	}
	switch state {
	case "held":
		pool.Status.BitMap, pool.Status.Free = mustBits(t, pool, 901)
		pool.Status.Leases["901"] = allocation.PoolLease{OwnerUID: "owner", Generation: 4}
	case "legacy-held":
		pool.Status.BitMap, pool.Status.Free = mustBits(t, pool, 901)
	case "retired":
		pool.Status.BitMap, pool.Status.Free = mustBits(t, pool, 901)
		pool.Status.Retired["901"] = allocation.PoolRetired{OwnerUID: "gone", Generation: 2}
	case "free":
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pool).Build()
	barriers := &atomic.Int32{}
	var raw []nativeFlow
	for _, v := range flowVNIs {
		raw = append(raw, nativeFlow{VNI: v, HasVNI: true, Raw: "table=6 actions=drop"})
	}
	flows := &FlowManager{client: scopeFlowAdapterWith(t, func() map[string]uint32 { return nil }, func() { barriers.Add(1) }), nativeFlowRead: func(context.Context) ([]nativeFlow, error) { return raw, nil }, floods: map[uint]t6Flood{}}
	l := &vniledger.Ledger{Client: c, Reader: c, Namespace: names.SystemNamespace, Node: "n1"}
	flows.Ledger = l
	for _, v := range ledger {
		if err := l.Note(v); err != nil {
			t.Fatal(err)
		}
	}
	return &orphanFixture{r: &VNIOrphanReconciler{Reader: c, OVS: &OVSManager{}, Flows: flows, Ledger: l, NodeName: "n1"}, c: c, barriers: barriers}
}

func mustBits(t *testing.T, p *allocation.Pool, bits ...uint) (string, uint) {
	t.Helper()
	out, f, err := poolpkg.SetBitsForTest(p.Status.BitMap, p.Status.Free, bits...)
	if err != nil {
		t.Fatal(err)
	}
	return out, f
}

func (f *orphanFixture) entries(t *testing.T) []uint {
	t.Helper()
	e, err := f.r.Ledger.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A node that comes back after a long absence: the lab is gone, its slot is retired (or already back in the pool), and the node still
// has the flows. The pass removes them and takes the number out of the ledger, so the operator can give the slot back.
func TestOrphanPassRemovesTheFlowsOfARetiredOrFreeSlot(t *testing.T) {
	for _, state := range []string{"retired", "free"} {
		t.Run(state, func(t *testing.T) {
			f := orphanTest(t, state, []uint64{901}, 901)
			dropped, undecided, err := f.r.Sweep(context.Background())
			if err != nil || dropped != 1 || undecided != 0 {
				t.Fatalf("dropped %d undecided %d err %v", dropped, undecided, err)
			}
			if f.barriers.Load() == 0 {
				t.Fatal("the flows were not removed from the switch")
			}
			if e := f.entries(t); len(e) != 0 {
				t.Fatalf("the ledger still has %v", e)
			}
		})
	}
}

// What a lease holds is not touched: not by the number, not by a legacy hold of an unknown owner.
func TestOrphanPassNeverTouchesAHeldSlot(t *testing.T) {
	for _, state := range []string{"held", "legacy-held"} {
		t.Run(state, func(t *testing.T) {
			f := orphanTest(t, state, []uint64{901}, 901)
			dropped, undecided, err := f.r.Sweep(context.Background())
			if err != nil || dropped != 0 || undecided != 0 {
				t.Fatalf("dropped %d undecided %d err %v", dropped, undecided, err)
			}
			if f.barriers.Load() != 0 {
				t.Fatal("flows of a held VNI were removed")
			}
			if e := f.entries(t); len(e) != 1 || e[0] != 901 {
				t.Fatalf("ledger = %v", e)
			}
		})
	}
}

// Flows the ledger never heard of (they were installed before it existed) are written into it first, then judged like the rest: held
// stays (and the pool now keeps the number from anyone else), an unowned one goes.
func TestOrphanPassAdoptsFlowsTheLedgerDoesNotKnow(t *testing.T) {
	held := orphanTest(t, "held", []uint64{901})
	if dropped, _, err := held.r.Sweep(context.Background()); err != nil || dropped != 0 {
		t.Fatalf("dropped %d err %v", dropped, err)
	}
	if e := held.entries(t); len(e) != 1 || e[0] != 901 {
		t.Fatalf("an unknown flow of a held VNI must enter the ledger: %v", e)
	}
	free := orphanTest(t, "retired", []uint64{901})
	if dropped, _, err := free.r.Sweep(context.Background()); err != nil || dropped != 1 {
		t.Fatalf("dropped %d err %v", dropped, err)
	}
	if e := free.entries(t); len(e) != 0 {
		t.Fatalf("ledger = %v", e)
	}
}

// Doubt leaves everything as it is: the pool and an object disagree, or the pool is unreadable.
func TestOrphanPassLeavesWhatItCannotJudge(t *testing.T) {
	f := orphanTest(t, "free", []uint64{901}, 901)
	vni := uint(901)
	if err := f.c.Create(context.Background(), &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns"}, Status: lab.DeviceStatus{VNI: &vni}}); err != nil {
		t.Fatal(err)
	}
	dropped, undecided, err := f.r.Sweep(context.Background())
	if err != nil || dropped != 0 || undecided != 1 || f.barriers.Load() != 0 {
		t.Fatalf("a Device that names a free slot: dropped %d undecided %d err %v", dropped, undecided, err)
	}
	// VNI outside every pool / pool missing
	g := orphanTest(t, "free", []uint64{4000000}, 4000000)
	dropped, undecided, err = g.r.Sweep(context.Background())
	if err != nil || dropped != 0 || undecided != 1 || g.barriers.Load() != 0 {
		t.Fatalf("an unreadable pool: dropped %d undecided %d err %v", dropped, undecided, err)
	}
}

// The race: the pass sees the slot retired, and before it removes anything the slot is issued again (to this node's own next lab). The
// slot is read again under the lock; the flows of the new owner stay.
func TestOrphanPassDoesNotRemoveTheFlowsOfAVNIIssuedAgain(t *testing.T) {
	f := orphanTest(t, "retired", []uint64{901}, 901)
	f.r.Reader = &flipAfterFirstPoolRead{Reader: f.c, flip: func() {
		ctx := context.Background()
		var p allocation.Pool
		if err := f.c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: names.SystemNamespace}, &p); err != nil {
			t.Error(err)
			return
		}
		delete(p.Status.Retired, "901")
		if p.Status.Leases == nil {
			p.Status.Leases = map[string]allocation.PoolLease{}
		}
		p.Status.Leases["901"] = allocation.PoolLease{OwnerUID: "new-owner", Generation: 9}
		if err := f.c.Update(ctx, &p); err != nil {
			t.Error(err)
		}
	}}
	dropped, _, err := f.r.Sweep(context.Background())
	if err != nil || dropped != 0 {
		t.Fatalf("dropped %d err %v", dropped, err)
	}
	if f.barriers.Load() != 0 {
		t.Fatal("flows of a VNI issued again were removed")
	}
	if e := f.entries(t); len(e) != 1 || e[0] != 901 {
		t.Fatalf("ledger = %v", e)
	}
}

// flipAfterFirstPoolRead answers the first read of a Pool as it was and runs flip right after it.
type flipAfterFirstPoolRead struct {
	client.Reader
	done bool
	flip func()
}

func (r *flipAfterFirstPoolRead) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*allocation.Pool); ok && !r.done {
		r.done = true
		r.flip()
	}
	return err
}

// The write-ahead: a flow of a VNI is installed only after the ledger has the number; a ledger that cannot be written stops the flow.
func TestFlowManagerRecordsTheVNIBeforeItsFlowsAndKeepsItUntilTheyAreGone(t *testing.T) {
	f := orphanTest(t, "held", nil)
	if err := f.r.Flows.RebuildT6Flood(901, nil, nil); err != nil {
		t.Fatal(err)
	}
	if e := f.entries(t); len(e) != 1 || e[0] != 901 {
		t.Fatalf("the VNI must be in the ledger once its flows are programmed: %v", e)
	}
	if err := f.r.Flows.RetireVNI(901); err != nil {
		t.Fatal(err)
	}
	if e := f.entries(t); len(e) != 0 {
		t.Fatalf("the VNI must leave the ledger after the flows are confirmed gone: %v", e)
	}
	f.r.Flows.Ledger = failingNote{}
	if err := f.r.Flows.RebuildT6Flood(902, nil, nil); err == nil {
		t.Fatal("flows were programmed although the VNI could not be recorded")
	}
}

type failingNote struct{}

func (failingNote) Note(uint) error  { return context.DeadlineExceeded }
func (failingNote) Clear(uint) error { return nil }

// A RetireVNI that starts after an install has recorded its VNI waits until the install is done; it cannot take the record out from
// between the Note and the flows (that would leave flows the ledger does not know).
func TestRetireWaitsForAnInstallThatHasRecordedItsVNI(t *testing.T) {
	f := orphanTest(t, "held", nil)
	noted, release := make(chan struct{}), make(chan struct{})
	f.r.Flows.Ledger = &pausingLedger{VNILedger: f.r.Ledger, noted: noted, release: release}
	installed, retired := make(chan error, 1), make(chan error, 1)
	go func() { installed <- f.r.Flows.RebuildT6Flood(901, nil, nil) }()
	<-noted
	go func() { retired <- f.r.Flows.RetireVNI(901) }()
	select {
	case <-retired:
		t.Fatal("RetireVNI finished between the record and the flows of an install")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-installed; err != nil {
		t.Fatal(err)
	}
	if err := <-retired; err != nil {
		t.Fatal(err)
	}
	if e := f.entries(t); len(e) != 0 {
		t.Fatalf("the retire ran after the install: the ledger must be empty, has %v", e)
	}
}

// pausingLedger tells when a VNI is recorded and holds the install there until released.
type pausingLedger struct {
	VNILedger
	noted, release chan struct{}
	once           sync.Once
}

func (l *pausingLedger) Note(vni uint) error {
	if err := l.VNILedger.Note(vni); err != nil {
		return err
	}
	l.once.Do(func() {
		close(l.noted)
		<-l.release
	})
	return nil
}
