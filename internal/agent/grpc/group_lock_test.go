package grpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	typed "github.com/cybericebox/laboratory/clientset/client/versioned/typed/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// rendezvousClientset makes every Lab create wait until `want` creates are inside at the same time (or `limit` passes): a
// serialized agent never gets there.
type rendezvousClientset struct {
	versioned.Interface
	r *rendezvous
}

type rendezvous struct {
	want    int32
	limit   time.Duration
	inside  atomic.Int32
	peak    atomic.Int32
	gate    chan struct{}
	once    sync.Once
	reached atomic.Bool
}

func (r *rendezvous) arrive() {
	n := r.inside.Add(1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if n >= r.want {
		r.reached.Store(true)
		r.once.Do(func() { close(r.gate) })
	}
	select {
	case <-r.gate:
	case <-time.After(r.limit):
	}
	r.inside.Add(-1)
}

func (c rendezvousClientset) LaboratoryV1alpha1() typed.LaboratoryV1alpha1Interface {
	return rendezvousLab{c.Interface.LaboratoryV1alpha1(), c.r}
}

type rendezvousLab struct {
	typed.LaboratoryV1alpha1Interface
	r *rendezvous
}

func (c rendezvousLab) Labs(ns string) typed.LabInterface {
	return rendezvousLabs{c.LaboratoryV1alpha1Interface.Labs(ns), c.r}
}

type rendezvousLabs struct {
	typed.LabInterface
	r *rendezvous
}

func (c rendezvousLabs) Create(ctx context.Context, l *laboratoryv1alpha1.Lab, o metav1.CreateOptions) (*laboratoryv1alpha1.Lab, error) {
	c.r.arrive()
	return c.LabInterface.Create(ctx, l, o)
}

// Labs of different groups are created at the same time; labs of one group go one after another.
func TestCreateLabsOfDifferentGroupsRunInParallel(t *testing.T) {
	h, k8s := newTestHandler(t)
	const groups = 6
	items := make([]*protobuf.LabItem, 0, groups)
	for i := range groups {
		g := fmt.Sprintf("team-%d", i)
		readyGroup(t, h, k8s, g, g, nil)
		items = append(items, &protobuf.LabItem{LabGroup: g, Name: "c1", VariantId: "plain"})
	}
	rv := &rendezvous{want: groups, limit: 8 * time.Second, gate: make(chan struct{})}
	h.cs = rendezvousClientset{Interface: h.cs, r: rv}

	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "plain", SpecJson: specJSON("box")}},
		Items:    items,
	})
	states := make([]protobuf.ItemState, groups)
	for i := range states {
		states[i] = stCreated
	}
	wantStates(t, res, err, states...)
	if !rv.reached.Load() || rv.peak.Load() < groups {
		t.Fatalf("only %d of %d creates were in flight together: the groups are created one after another", rv.peak.Load(), groups)
	}
}

func TestCreateLabsOfOneGroupAreSerialized(t *testing.T) {
	h, k8s := newTestHandler(t)
	readyGroup(t, h, k8s, "team-x", "team-x", nil)
	rv := &rendezvous{want: 2, limit: 300 * time.Millisecond, gate: make(chan struct{})}
	h.cs = rendezvousClientset{Interface: h.cs, r: rv}
	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "plain", SpecJson: specJSON("box")}},
		Items: []*protobuf.LabItem{
			{LabGroup: "team-x", Name: "c1", VariantId: "plain"},
			{LabGroup: "team-x", Name: "c2", VariantId: "plain"},
		},
	})
	wantStates(t, res, err, stCreated, stCreated)
	if rv.peak.Load() > 1 {
		t.Fatalf("%d labs of one group were admitted at the same time: the group has one admission slot", rv.peak.Load())
	}
}

func TestGroupLockExcludesOnlyTheSameGroup(t *testing.T) {
	h := &Handler{}
	unlockA := h.groupLock("team-a")
	done := make(chan struct{})
	go func() {
		h.groupLock("team-b")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("group b waited for group a")
	}
	got := make(chan struct{})
	go func() {
		h.groupLock("team-a")()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("the same group was entered twice")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the group lock was not released")
	}
	if h.resLocks.held() != 0 {
		t.Fatalf("%d group locks left behind", h.resLocks.held())
	}
}
