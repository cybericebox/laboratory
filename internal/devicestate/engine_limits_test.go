package devicestate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func randomImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// a clock the tests move
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func rigWithClock(t *testing.T) (*rig, *testClock) {
	r := newRig(t, time.Hour, 1<<20)
	clk := &testClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	r.e.Now = clk.now
	return r, clk
}

func push(t *testing.T, r *rig, tr *tracked, file, body string) error {
	t.Helper()
	r.rt.setDiff(tarOf(map[string]string{file: body}))
	return tr.snapshot(context.Background(), true)
}

// R-5: one device cannot push without pause; what it pushes in an hour is bounded; the exit snapshot is never held back.
func TestEngineRateLimitsPushesPerDevice(t *testing.T) {
	r, clk := rigWithClock(t)
	r.e.MinPushInterval = time.Minute
	tr := r.track(t)

	if err := push(t, r, tr, "a", "1"); err != nil {
		t.Fatal(err)
	}
	err := push(t, r, tr, "b", "2")
	var def *errDeferred
	if !errors.As(err, &def) || def.after <= 0 || def.after > time.Minute {
		t.Fatalf("a second push at once must be deferred: %v", err)
	}
	if recs, warns, _ := r.cl.snapshot(); len(recs) != 1 || len(warns) != 0 {
		t.Fatalf("a deferral is not a failure and not a warning: %v %v", recs, warns)
	}
	clk.advance(61 * time.Second)
	if err := push(t, r, tr, "b", "2"); err != nil {
		t.Fatalf("after the interval: %v", err)
	}
	// the exit snapshot is exempt: it must not be lost
	r.rt.setDiff(tarOf(map[string]string{"c": "3"}))
	if err := tr.snapshot(context.Background(), false); err != nil {
		t.Fatalf("exit snapshot: %v", err)
	}
	if recs, _, _ := r.cl.snapshot(); len(recs) != 3 {
		t.Fatalf("records: %d", len(recs))
	}
}

func TestEnginePushBudgetPerWindow(t *testing.T) {
	r, clk := rigWithClock(t)
	r.e.PushBudget = 1 // the first push is over it
	r.e.PushBudgetWindow = time.Hour
	tr := r.track(t)
	if err := push(t, r, tr, "a", "1"); err != nil {
		t.Fatal(err)
	}
	err := push(t, r, tr, "b", "2")
	var def *errDeferred
	if !errors.As(err, &def) || !strings.Contains(def.reason, "budget") {
		t.Fatalf("the budget must defer the next push: %v", err)
	}
	if def.after > time.Hour || def.after < 59*time.Minute {
		t.Fatalf("it waits until the first push leaves the window: %s", def.after)
	}
	clk.advance(time.Hour + time.Second)
	if err := push(t, r, tr, "b", "2"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

type fullGuard struct{ err error }

func (g fullGuard) Check(context.Context, int64) error { return g.err }

func TestEngineRefusesToPushIntoAFullRegistry(t *testing.T) {
	r, _ := rigWithClock(t)
	r.e.Space = fullGuard{err: snapshot.ErrRegistryFull}
	tr := r.track(t)
	err := push(t, r, tr, "a", "1")
	var def *errDeferred
	if !errors.As(err, &def) || def.after < time.Minute {
		t.Fatalf("a live snapshot is retried later: %v", err)
	}
	recs, warns, _ := r.cl.snapshot()
	if len(recs) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "nearly full") {
		t.Fatalf("nothing pushed, the status says why: %v %v", recs, warns)
	}
	// the exit snapshot is given up quietly (the last good one stays)
	r.rt.setDiff(tarOf(map[string]string{"b": "2"}))
	if err := tr.snapshot(context.Background(), false); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if recs, _, _ := r.cl.snapshot(); len(recs) != 0 {
		t.Fatal("nothing may be pushed while the registry is full")
	}
	// room again
	r.e.Space = fullGuard{}
	if err := push(t, r, tr, "c", "3"); err != nil {
		t.Fatal(err)
	}
}

// Each snapshot replaces the previous one in the registry: only the current manifest of the device is kept.
func TestEngineSupersedesThePreviousSnapshot(t *testing.T) {
	r, _ := rigWithClock(t)
	tr := r.track(t)
	ctx := context.Background()
	if err := push(t, r, tr, "a", "1"); err != nil {
		t.Fatal(err)
	}
	recs, _, _ := r.cl.snapshot()
	firstRef := recs[0].Image
	if err := push(t, r, tr, "b", "2"); err != nil {
		t.Fatal(err)
	}
	recs, _, _ = r.cl.snapshot()
	secondRef := recs[1].Image
	head := func(ref string) error {
		_, digest, _ := strings.Cut(ref, "@")
		h, _ := v1.NewHash(digest)
		target, _ := r.reg.Repo(r.pod.Repo)
		_, err := remote.Head(target.Digest(h.String()), r.reg.Options(ctx)...)
		return err
	}
	if err := head(firstRef); err == nil {
		t.Fatal("the superseded snapshot must be gone from the registry")
	}
	if err := head(secondRef); err != nil {
		t.Fatalf("the current one stays: %v", err)
	}
}

func TestEngineKeepsThePreviousSnapshotForTheGrace(t *testing.T) {
	r, _ := rigWithClock(t)
	r.e.SupersededGrace = 150 * time.Millisecond
	tr := r.track(t)
	ctx := context.Background()
	if err := push(t, r, tr, "a", "1"); err != nil {
		t.Fatal(err)
	}
	recs, _, _ := r.cl.snapshot()
	first := recs[0].Image
	if err := push(t, r, tr, "b", "2"); err != nil {
		t.Fatal(err)
	}
	_, digest, _ := strings.Cut(first, "@")
	h, _ := v1.NewHash(digest)
	target, _ := r.reg.Repo(r.pod.Repo)
	has := func() bool {
		_, err := remote.Head(target.Digest(h.String()), r.reg.Options(ctx)...)
		return err == nil
	}
	if !has() {
		t.Fatal("within the grace a pod that was just created from the old snapshot can still pull it")
	}
	deadline := time.Now().Add(5 * time.Second)
	for has() {
		if time.Now().After(deadline) {
			t.Fatal("the superseded snapshot must be deleted after the grace")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The tenant's quota counts what its retired labs still hold.
func TestEngineQuotaCountsRetainedRepositories(t *testing.T) {
	r, _ := rigWithClock(t)
	r.pod.TenantQuota = 1000
	r.pod.Tenant = "acme"
	r.cl.pods = []PodInfo{r.pod}
	r.e.Retained = r.reg
	// a lab of the tenant is gone; its repository keeps 990 bytes
	gone := snapshot.Repo("ns", "gone", "web")
	img := snapshot.Annotated(randomImage(t), "acme", 990)
	if _, _, err := r.reg.Push(context.Background(), gone, img, 0, ""); err != nil {
		t.Fatal(err)
	}
	tr := r.track(t)
	r.rt.setDiff(tarOf(map[string]string{"a": strings.Repeat("x", 100)}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recs, warns, _ := r.cl.snapshot()
	if len(recs) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "quota") {
		t.Fatalf("the retained 990 bytes plus this snapshot pass 1000: %v %v", recs, warns)
	}
}
