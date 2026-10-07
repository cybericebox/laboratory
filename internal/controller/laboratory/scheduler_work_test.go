package laboratory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type schedulerReadCounter struct {
	client.Client
	pullGets int
}

func TestSchedulerEmptyNodePrepullUsesRequiredArray(t *testing.T) {
	pull := buildImagePull("empty-nodes", "tenant", []string{"image:v1"}, nil)
	data, err := json.Marshal(pull)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	spec := body["spec"].(map[string]any)
	if nodes, ok := spec["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Fatalf("required nodes must be an empty array, got %v", spec["nodes"])
	}
	if !imagePullProgress(pull).done {
		t.Fatal("zero-node prepull must remain immediately complete")
	}
}

func (c *schedulerReadCounter) Get(ctx context.Context, key client.ObjectKey, out client.Object, opts ...client.GetOption) error {
	if _, ok := out.(*api.ImagePull); ok {
		c.pullGets++
	}
	return c.Client.Get(ctx, key, out, opts...)
}
func TestSchedulerSharedPendingPrepullReadOncePerPass(t *testing.T) {
	cfg := schedCfg()
	cfg.Prepull = true
	f := newSchedFixture(t, cfg)
	f.addLab("a", "shared", nil, "one")
	f.addLab("b", "shared", nil, "one")
	key := prepClass(names.TenantOf(nil), "g/shared")
	f.createPlain(&api.ImagePull{ObjectMeta: metav1.ObjectMeta{Name: prepullName(key), CreationTimestamp: metav1.NewTime(f.now), Labels: map[string]string{prepullLabel: prepullKey(key)}}, Spec: api.ImagePullSpec{Nodes: []string{"node"}}})
	counter := &schedulerReadCounter{Client: f.c}
	f.s.Client = counter
	f.tick()
	if counter.pullGets != 1 {
		t.Fatalf("same pending prepull read %d times in a pass", counter.pullGets)
	}
	counter.pullGets = 0
	f.tick()
	if counter.pullGets != 1 {
		t.Fatal("pending progress was not checked again next pass")
	}
}
func TestSchedulerObserveStartedAllocationDoesNotScaleWithDevices(t *testing.T) {
	allocations := func(n int) float64 {
		snap := &clusterView{devices: map[string]*api.Device{}, suspended: map[string]bool{}}
		for i := 0; i < n; i++ {
			snap.devices[string(rune(i))] = &api.Device{Spec: api.DeviceSpec{Type: api.DeviceTypeContainer}, Status: api.DeviceStatus{Scheduling: &api.PodSchedule{State: api.PodStarted}}}
		}
		s := &Scheduler{}
		return testing.AllocsPerRun(10, func() { s.observe(context.Background(), snap, time.Now()) })
	}
	small, large := allocations(1), allocations(100)
	if large > small+10 {
		t.Fatalf("already started observation allocates per device: small=%v large=%v", small, large)
	}
}
func TestSchedulerRecentExpiryKeepsPreparedAndYoungDispatch(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.tick()
	f.s.prepared["remembered-class"] = struct{}{}
	f.s.recent["expired-deleted-device"] = f.now.Add(-time.Hour)
	f.s.recent["young-missing-device"] = f.now
	f.tick()
	if _, ok := f.s.recent["expired-deleted-device"]; ok {
		t.Fatal("expired deleted device remains retained")
	}
	if _, ok := f.s.recent["young-missing-device"]; !ok {
		t.Fatal("young dispatch protection was dropped")
	}
	if _, ok := f.s.prepared["remembered-class"]; !ok {
		t.Fatal("completed class history was dropped")
	}
}
