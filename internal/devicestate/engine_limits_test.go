package devicestate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

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

// fakeGate plays the refusals of the registry gate in front of an ordinary registry: while a refusal is set, the push of a manifest
// (PUT) is answered with its status, its CIB_* code and its Retry-After, as the gate answers it.
type fakeGate struct {
	inner http.Handler
	mu    sync.Mutex
	rule  *gateAnswer
	hits  int
}

type gateAnswer struct {
	status int
	code   string
	retry  int // seconds, 0: no header
	msg    string
}

func (g *fakeGate) refuse(a *gateAnswer) { g.mu.Lock(); g.rule = a; g.mu.Unlock() }

func (g *fakeGate) refusals() int { g.mu.Lock(); defer g.mu.Unlock(); return g.hits }

func (g *fakeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	rule := g.rule
	if rule != nil && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
		g.hits++
	} else {
		rule = nil
	}
	g.mu.Unlock()
	if rule == nil {
		g.inner.ServeHTTP(w, r)
		return
	}
	if rule.retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(rule.retry))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rule.status)
	_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":%q}]}`, rule.code, rule.msg)
}

// 429 with Retry-After (the least interval, the push budget, the exit limit) is a deferral by the time the gate names: not a failure and not
// a warning, and the snapshot is taken when the gate lets it.
func TestEngineDefersWhatTheGateRateLimits(t *testing.T) {
	for _, code := range []string{snapshot.CodePushInterval, snapshot.CodePushBudget, snapshot.CodeExitLimit} {
		t.Run(code, func(t *testing.T) {
			r, _ := rigWithClock(t)
			tr := r.track(t)
			r.gate.refuse(&gateAnswer{status: http.StatusTooManyRequests, code: code, retry: 42, msg: "slow down"})
			err := push(t, r, tr, "a", "1")
			var def *errDeferred
			if !errors.As(err, &def) || def.after != 42*time.Second || !strings.Contains(def.reason, code) {
				t.Fatalf("the gate's Retry-After is the wait: %v", err)
			}
			if recs, warns, _ := r.cl.snapshot(); len(recs) != 0 || len(warns) != 0 {
				t.Fatalf("a deferral is not a failure and not a warning: %v %v", recs, warns)
			}
			r.gate.refuse(nil)
			if err := push(t, r, tr, "a", "1"); err != nil {
				t.Fatalf("when the gate lets it: %v", err)
			}
			if recs, _, _ := r.cl.snapshot(); len(recs) != 1 {
				t.Fatalf("records: %d", len(recs))
			}
		})
	}
}

// A 429 without Retry-After waits a minute at least; a 429 with a silly one waits a second at least.
func TestEngineDeferralHasBounds(t *testing.T) {
	r, _ := rigWithClock(t)
	tr := r.track(t)
	r.gate.refuse(&gateAnswer{status: http.StatusTooManyRequests, code: snapshot.CodePushInterval, msg: "x"})
	var def *errDeferred
	if err := push(t, r, tr, "a", "1"); !errors.As(err, &def) || def.after != time.Minute {
		t.Fatalf("no Retry-After: %v", err)
	}
}

// 507 CIB_REGISTRY_FULL: the live snapshot is retried later and the status says why; the exit snapshot is given up quietly (the last good one
// stays); nothing is pushed while the gate refuses, and the next push after room is made goes through.
func TestEngineRefusesToPushIntoAFullRegistry(t *testing.T) {
	r, _ := rigWithClock(t)
	tr := r.track(t)
	r.gate.refuse(&gateAnswer{status: http.StatusInsufficientStorage, code: snapshot.CodeRegistryFull, msg: "the snapshot registry is nearly full"})
	err := push(t, r, tr, "a", "1")
	var def *errDeferred
	if !errors.As(err, &def) || def.after < time.Minute {
		t.Fatalf("a live snapshot is retried later: %v", err)
	}
	recs, warns, _ := r.cl.snapshot()
	if len(recs) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "nearly full") || !strings.Contains(warns[0], snapshot.CodeRegistryFull) {
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
	r.gate.refuse(nil)
	if err := push(t, r, tr, "c", "3"); err != nil {
		t.Fatal(err)
	}
}

// 403 CIB_TENANT_QUOTA: the layer is not offered again as it is, the status says it, and a required capture fails with the quota error.
func TestEngineMapsTheTenantQuotaOfTheGate(t *testing.T) {
	r, _ := rigWithClock(t)
	tr := r.track(t)
	r.gate.refuse(&gateAnswer{status: http.StatusForbidden, code: snapshot.CodeTenantQuota, msg: "the snapshots of the tenant would take more than its quota"})
	if err := push(t, r, tr, "a", "1"); err != nil {
		t.Fatalf("a quota refusal is a warning, not an error: %v", err)
	}
	recs, warns, _ := r.cl.snapshot()
	if len(recs) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "quota") {
		t.Fatalf("%v %v", recs, warns)
	}
	hits := r.gate.refusals()
	if err := tr.snapshot(context.Background(), true); err != nil || r.gate.refusals() != hits {
		t.Fatalf("the same layer must not be pushed again: %v (%d requests)", err, r.gate.refusals()-hits)
	}
	// a required capture gets the error
	r.rt.setDiff(tarOf(map[string]string{"b": "2"}))
	err := tr.snapshot(context.Background(), false)
	if err != nil {
		t.Fatalf("the exit snapshot of a pod over its quota is a warning too: %v", err)
	}
}

// Another failure of a push is still a failure, with the retry the engine has always had.
func TestEngineKeepsFailuresThatAreNotTheGatesRules(t *testing.T) {
	r, _ := rigWithClock(t)
	tr := r.track(t)
	r.gate.refuse(&gateAnswer{status: http.StatusInternalServerError, code: "UNKNOWN", msg: "boom"})
	err := push(t, r, tr, "a", "1")
	var def *errDeferred
	if err == nil || errors.As(err, &def) || !strings.Contains(err.Error(), "push snapshot") {
		t.Fatalf("%v", err)
	}
}

// 503 with Retry-After (the gate's view of the cluster is not in contact) is waited out by the time the gate names, and the codes that ask to
// start the push again (a cancelled upload session, a blob that is not there yet) defer it by a few seconds: neither fails the device.
func TestEngineHonoursUnavailableAndRestartAnswers(t *testing.T) {
	cases := map[string]struct {
		answer gateAnswer
		after  time.Duration
	}{
		"503 with Retry-After": {gateAnswer{status: http.StatusServiceUnavailable, code: "UNAVAILABLE", retry: 30, msg: "the cluster view is not available"}, 30 * time.Second},
		"503 without":          {gateAnswer{status: http.StatusServiceUnavailable, code: "UNAVAILABLE", msg: "x"}, unavailableWait},
		"upload unknown":       {gateAnswer{status: http.StatusNotFound, code: snapshot.CodeBlobUploadUnknown, msg: "unknown upload session"}, restartWait},
		"manifest blob":        {gateAnswer{status: http.StatusBadRequest, code: snapshot.CodeManifestBlobUnknown, msg: "a blob is missing"}, restartWait},
		"upload limit":         {gateAnswer{status: http.StatusTooManyRequests, code: snapshot.CodeUploadLimit, retry: 5, msg: "too many uploads"}, 5 * time.Second},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := rigWithClock(t)
			tr := r.track(t)
			a := c.answer
			r.gate.refuse(&a)
			err := push(t, r, tr, "a", "1")
			var def *errDeferred
			if !errors.As(err, &def) || def.after != c.after {
				t.Fatalf("%v", err)
			}
			if recs, warns, _ := r.cl.snapshot(); len(recs) != 0 || len(warns) != 0 {
				t.Fatalf("a deferral is not a failure and not a warning: %v %v", recs, warns)
			}
		})
	}
}

// A blob without a live grant (404 BLOB_UNKNOWN) makes the read ask for the manifest, which makes the grant, and then ask for the blob again;
// a read that still does not go through is a deferral, and a 503 with Retry-After is one by the gate's time.
func TestPackedLayerReadRestartsAfterBlobUnknown(t *testing.T) {
	img := randomImageForGate(t)
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	md, _ := img.Digest()
	var mu sync.Mutex
	granted, manifestAsked, blobAsked := false, 0, 0
	mode := "grant"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			manifestAsked++
			granted = true
			raw, _ := img.RawManifest()
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Docker-Content-Digest", md.String())
			_, _ = w.Write(raw)
		case strings.Contains(r.URL.Path, "/blobs/"):
			blobAsked++
			switch {
			case mode == "unavailable":
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"errors":[{"code":"UNAVAILABLE","message":"not in contact"}]}`)
			case mode == "never" || !granted:
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"errors":[{"code":"BLOB_UNKNOWN","message":"ask for the manifest first"}]}`)
			default:
				rc, _ := layers[0].Compressed()
				_, _ = io.Copy(w, rc)
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://")+"/c/acme/docker.io/library/app", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := readPackedLayer(context.Background(), repo, md, nil, http.DefaultTransport, ld)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	mu.Lock()
	if manifestAsked != 1 || blobAsked != 2 {
		t.Errorf("the manifest is asked once and the blob twice: %d %d", manifestAsked, blobAsked)
	}
	granted, mode = false, "never"
	mu.Unlock()
	_, err = readPackedLayer(context.Background(), repo, md, nil, http.DefaultTransport, ld)
	var def *errDeferred
	if !errors.As(err, &def) || def.after != restartWait {
		t.Fatalf("a read that still does not go through is deferred: %v", err)
	}
	mu.Lock()
	mode = "unavailable"
	mu.Unlock()
	_, err = readPackedLayer(context.Background(), repo, md, nil, http.DefaultTransport, ld)
	if !errors.As(err, &def) || def.after != 17*time.Second {
		t.Fatalf("503 is waited out by the gate's time: %v", err)
	}
}

func randomImageForGate(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	return img
}
