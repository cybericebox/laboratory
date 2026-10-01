package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func tarOf(files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

type fakeRuntime struct {
	mu     sync.Mutex
	upper  string
	diff   []byte
	images map[string]v1.Image
	freeze []bool
	gone   bool
	// failDiff is the number of Diff calls that fail before one succeeds.
	failDiff int
	diffs    int
}

func (f *fakeRuntime) Inspect(_ context.Context, id string) (Container, error) {
	if f.gone {
		return Container{}, fmt.Errorf("container %s not found", id)
	}
	return Container{ID: id, ImageRef: "docker.io/library/app:1", UpperDir: f.upper}, nil
}

func (f *fakeRuntime) Diff(_ context.Context, _ Container, freeze bool) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freeze = append(f.freeze, freeze)
	f.diffs++
	if f.failDiff > 0 {
		f.failDiff--
		return nil, fmt.Errorf("content digest sha256:x: not found")
	}
	return io.NopCloser(bytes.NewReader(f.diff)), nil
}

func (f *fakeRuntime) LoadImage(_ context.Context, ref string) (v1.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img, ok := f.images[ref]; ok {
		return img, nil
	}
	return nil, fmt.Errorf("no image %s", ref)
}

func (f *fakeRuntime) Exits(ctx context.Context) (<-chan string, error) {
	return make(chan string), nil
}

func (f *fakeRuntime) setDiff(b []byte) {
	f.mu.Lock()
	f.diff = b
	f.mu.Unlock()
}

type fakeCluster struct {
	mu      sync.Mutex
	pods    []PodInfo
	records []Snapshot
	warns   []string
	exits   []string
}

func (c *fakeCluster) Pods(context.Context) ([]PodInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]PodInfo(nil), c.pods...), nil
}
func (c *fakeCluster) Record(_ context.Context, _ PodInfo, s Snapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, s)
	return nil
}
func (c *fakeCluster) Warn(_ context.Context, _ PodInfo, m string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warns = append(c.warns, m)
	return nil
}
func (c *fakeCluster) MarkExit(_ context.Context, p PodInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exits = append(c.exits, p.Pod)
	return nil
}
func (c *fakeCluster) snapshot() (recs []Snapshot, warns, exits []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Snapshot(nil), c.records...), append([]string(nil), c.warns...), append([]string(nil), c.exits...)
}

type rig struct {
	e   *Engine
	rt  *fakeRuntime
	cl  *fakeCluster
	reg *snapshot.Registry
	pod PodInfo
}

func newRig(t *testing.T, debounce time.Duration, maxBytes int64) *rig {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	reg := &snapshot.Registry{Host: strings.TrimPrefix(srv.URL, "http://")}
	base, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{upper: t.TempDir(), images: map[string]v1.Image{"docker.io/library/app:1": base}}
	cl := &fakeCluster{}
	pod := PodInfo{
		Device: types.NamespacedName{Namespace: "ns", Name: "lab-web"}, Pod: "lab-web-1", Incarnation: 1,
		ContainerID: "c1", Running: true,
		Policy: snapshot.NewPolicy(debounce, nil, maxBytes, 10),
		Repo:   snapshot.Repo("ns", "lab", "web"),
	}
	cl.pods = []PodInfo{pod}
	e := &Engine{
		Runtime: rt, Cluster: cl, Pusher: reg, RegistryHost: reg.Host, WorkDir: t.TempDir(),
		Poll: time.Hour, Log: logr.Discard(), ExitTimeout: 10 * time.Second,
	}
	e.tracked = map[string]*tracked{}
	return &rig{e: e, rt: rt, cl: cl, reg: reg, pod: pod}
}

func (r *rig) track(t *testing.T) *tracked {
	t.Helper()
	r.e.Sync(context.Background())
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	tr := r.e.tracked["c1"]
	if tr == nil {
		t.Fatal("container not followed after Sync")
	}
	return tr
}

func TestEngineSnapshotsRunningDeviceAfterDebounce(t *testing.T) {
	r := newRig(t, 50*time.Millisecond, 1<<20)
	r.rt.setDiff(tarOf(map[string]string{"data/db": "rows", "tmp/junk": "xxxxxxxx"}))
	r.e.Sync(context.Background())
	defer r.e.stopAll()

	deadline := time.Now().Add(5 * time.Second)
	for {
		recs, _, _ := r.cl.snapshot()
		if len(recs) > 0 {
			rec := recs[0]
			if !strings.HasPrefix(rec.Image, r.reg.Host+"/lab/ns/lab/web@sha256:") || rec.Layers != 1 || rec.SizeBytes != 4 {
				t.Fatalf("unexpected record %+v (tmp must be excluded, size is the kept bytes)", rec)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no snapshot recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !r.rt.freeze[0] {
		t.Fatal("a running container is frozen for its snapshot")
	}
}

func TestEngineExitSnapshotDoesNotFreezeAndMarksExit(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	tr := r.track(t)
	r.rt.setDiff(tarOf(map[string]string{"srv/state": "final"}))
	r.e.finish(context.Background(), tr)

	recs, warns, exits := r.cl.snapshot()
	if len(recs) != 1 || len(warns) != 0 || len(exits) != 1 || exits[0] != "lab-web-1" {
		t.Fatalf("records %v warns %v exits %v", recs, warns, exits)
	}
	if len(r.rt.freeze) != 1 || r.rt.freeze[0] {
		t.Fatalf("exit snapshot must not freeze: %v", r.rt.freeze)
	}
	// A second exit signal (event plus resync) is ignored.
	r.e.finish(context.Background(), tr)
	if _, _, exits = r.cl.snapshot(); len(exits) != 1 {
		t.Fatal("exit snapshot must run once")
	}
}

func TestEngineQuotaKeepsLastSnapshotAndWarns(t *testing.T) {
	r := newRig(t, time.Hour, 10)
	tr := r.track(t)
	r.rt.setDiff(tarOf(map[string]string{"big": strings.Repeat("x", 100)}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recs, warns, _ := r.cl.snapshot()
	if len(recs) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "quota") {
		t.Fatalf("records %v warns %v", recs, warns)
	}
	// The same refused layer is not retried or re-reported.
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, warns, _ = r.cl.snapshot(); len(warns) != 1 {
		t.Fatalf("warning must not repeat: %v", warns)
	}
}

func TestEngineSkipsUnchangedLayerAndReturnsToBase(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	tr := r.track(t)
	ctx := context.Background()

	r.rt.setDiff(tarOf(map[string]string{}))
	if err := tr.snapshot(ctx, true); err != nil {
		t.Fatal(err)
	}
	if recs, _, _ := r.cl.snapshot(); len(recs) != 0 {
		t.Fatal("an unchanged device needs no snapshot")
	}

	r.rt.setDiff(tarOf(map[string]string{"a": "1"}))
	for i := 0; i < 2; i++ {
		if err := tr.snapshot(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	if recs, _, _ := r.cl.snapshot(); len(recs) != 1 {
		t.Fatalf("the same layer must be pushed once, got %d records", len(recs))
	}

	// The user deleted everything again: the state is the base image.
	r.rt.setDiff(tarOf(map[string]string{}))
	if err := tr.snapshot(ctx, true); err != nil {
		t.Fatal(err)
	}
	recs, _, _ := r.cl.snapshot()
	if len(recs) != 2 || recs[1].Image != "" || recs[1].Layers != 0 {
		t.Fatalf("a layer back to its start must record the base state, got %+v", recs)
	}
}

func TestEngineSyncIgnoresStaleEpochAndFinishesEndedPods(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	stale := r.pod
	stale.Epoch, stale.DeviceEpoch = 0, 1
	r.cl.pods = []PodInfo{stale}
	r.e.Sync(context.Background())
	if len(r.e.tracked) != 0 {
		t.Fatal("a pod of an old epoch must not be followed")
	}

	// A pod that ended while the node-agent was down still gets its exit snapshot.
	ended := r.pod
	ended.Running, ended.Ended = false, true
	r.cl.pods = []PodInfo{ended}
	r.rt.setDiff(tarOf(map[string]string{"x": "y"}))
	r.e.Sync(context.Background())
	recs, _, exits := r.cl.snapshot()
	if len(recs) != 1 || len(exits) != 1 {
		t.Fatalf("records %v exits %v", recs, exits)
	}

	// An ended pod whose container is already gone is marked so the controller does not wait.
	r2 := newRig(t, time.Hour, 1<<20)
	e2 := r2.pod
	e2.Running, e2.Ended = false, true
	r2.cl.pods = []PodInfo{e2}
	r2.rt.gone = true
	r2.e.Sync(context.Background())
	if recs, _, exits = r2.cl.snapshot(); len(recs) != 0 || len(exits) != 1 {
		t.Fatalf("records %v exits %v", recs, exits)
	}
}

func TestEngineSquashesAfterMaxLayers(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	r.pod.Policy = snapshot.NewPolicy(time.Hour, nil, 1<<20, 2)
	r.cl.pods = []PodInfo{r.pod}
	ctx := context.Background()

	// Three "restarts": each run starts from the previous snapshot and adds a layer.
	var lastLayers int32
	for i := 1; i <= 3; i++ {
		r.e.stopAll()
		r.e.tracked = map[string]*tracked{}
		tr := r.track(t)
		r.rt.setDiff(tarOf(map[string]string{fmt.Sprintf("f%d", i): "v"}))
		if err := tr.snapshot(ctx, false); err != nil {
			t.Fatal(err)
		}
		recs, _, _ := r.cl.snapshot()
		last := recs[len(recs)-1]
		lastLayers = last.Layers
		// The next run starts from this snapshot.
		img, err := r.reg.Image(ctx, r.pod.Repo)
		if err != nil {
			t.Fatal(err)
		}
		r.rt.mu.Lock()
		r.rt.images["docker.io/library/app:1"] = img
		r.rt.mu.Unlock()
	}
	if lastLayers != 1 {
		t.Fatalf("the third snapshot exceeds maxLayers=2 and squashes to one layer, got %d", lastLayers)
	}
}

func TestEngineWarnsAboutFilesSkippedForSize(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	r.cl.pods[0].Policy = r.cl.pods[0].Policy.WithMaxFileSize(10)
	tr := r.track(t)
	r.rt.setDiff(tarOf(map[string]string{"data/big": strings.Repeat("x", 100), "data/small": "ok"}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recs, warns, _ := r.cl.snapshot()
	if len(recs) != 1 || recs[0].SizeBytes != 2 {
		t.Fatalf("the small file is snapshotted, the big one is not: %+v", recs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "/data/big (100 bytes)") || strings.Contains(warns[0], "small") {
		t.Fatalf("warnings %v", warns)
	}
	// A change of the big file alone leaves the snapshot as it is and keeps the warning.
	r.rt.setDiff(tarOf(map[string]string{"data/big": strings.Repeat("y", 200)}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recs, warns, _ = r.cl.snapshot()
	if len(recs) != 2 {
		t.Fatalf("records %+v", recs)
	}
	if got := warns[len(warns)-1]; !strings.Contains(got, "/data/big (200 bytes)") {
		t.Fatalf("the new size is reported: %v", warns)
	}
}

func TestRetryDelayDoublesAndCaps(t *testing.T) {
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := retryDelay(10*time.Second, 5*time.Minute, i+1); got != w {
			t.Errorf("retry %d: got %v want %v", i+1, got, w)
		}
	}
}

// A snapshot that failed is taken again by itself, without a change of the layer.
func TestEngineRetriesAFailedSnapshotByItself(t *testing.T) {
	r := newRig(t, 20*time.Millisecond, 1<<20)
	r.e.RetryBase, r.e.RetryMax = 30*time.Millisecond, 100*time.Millisecond
	r.rt.failDiff = 3
	r.rt.setDiff(tarOf(map[string]string{"data/db": "rows"}))
	r.e.Sync(context.Background())
	defer r.e.stopAll()

	deadline := time.Now().Add(10 * time.Second)
	for {
		recs, _, _ := r.cl.snapshot()
		if len(recs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			r.rt.mu.Lock()
			defer r.rt.mu.Unlock()
			t.Fatalf("no snapshot after %d attempts", r.rt.diffs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.rt.mu.Lock()
	defer r.rt.mu.Unlock()
	if r.rt.diffs != 4 {
		t.Fatalf("want 3 failures then 1 success, got %d attempts", r.rt.diffs)
	}
}
