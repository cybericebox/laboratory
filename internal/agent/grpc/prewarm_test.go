package grpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// manifestFetches counts the manifest requests the cache saw for a repository.
func (z *fakeZot) manifestFetches(repo string) int {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := 0
	for k, c := range z.manifests {
		if strings.HasPrefix(k, repo+"@") {
			n += c
		}
	}
	return n
}

// fakeZot imitates the image cache: a manifest request for an image it can fetch
// "upstream" makes it store the manifest and, depending on mode, the blobs.
type fakeZot struct {
	mu        sync.Mutex
	upstream  map[string]v1.Image // "<repo>@<digest>" the cache can fetch
	indexes   map[string]v1.ImageIndex
	synced    map[string]bool
	blobs     map[string]bool // "<repo>/<digest>"
	manifests map[string]int  // manifest requests per "<repo>@<digest>"
	blobsToo  bool            // false: only the manifest is stored (a broken sync)
	gate      chan struct{}   // when set, manifest requests wait for it
	inflight  int
	maxFlight int
	srv       *httptest.Server
}

func newFakeZot(t *testing.T) *fakeZot {
	z := &fakeZot{upstream: map[string]v1.Image{}, indexes: map[string]v1.ImageIndex{}, synced: map[string]bool{},
		blobs: map[string]bool{}, manifests: map[string]int{}, blobsToo: true}
	z.srv = httptest.NewServer(z)
	t.Cleanup(z.srv.Close)
	return z
}

func (z *fakeZot) count(key string) int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.manifests[key]
}

func (z *fakeZot) manifestsCopy() map[string]int {
	z.mu.Lock()
	defer z.mu.Unlock()
	out := map[string]int{}
	for k, v := range z.manifests {
		out[k] = v
	}
	return out
}

func (z *fakeZot) hasBlob(key string) bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.blobs[key]
}

func (z *fakeZot) addr() string { return strings.TrimPrefix(z.srv.URL, "http://") }

func (z *fakeZot) addImage(t *testing.T, repo string) v1.Hash {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	z.upstream[repo+"@"+d.String()] = img
	return d
}

func (z *fakeZot) addIndex(t *testing.T, repo string) v1.Hash {
	t.Helper()
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := idx.Digest()
	z.indexes[repo+"@"+d.String()] = idx
	m, _ := idx.IndexManifest()
	for _, c := range m.Manifests {
		img, _ := idx.Image(c.Digest)
		z.upstream[repo+"@"+c.Digest.String()] = img
	}
	return d
}

func (z *fakeZot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	if p == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if i := strings.LastIndex(p, "/manifests/"); i >= 0 {
		z.manifest(w, p[:i], p[i+len("/manifests/"):])
		return
	}
	if i := strings.LastIndex(p, "/blobs/"); i >= 0 {
		z.mu.Lock()
		ok := z.blobs[p[:i]+"/"+p[i+len("/blobs/"):]]
		z.mu.Unlock()
		if ok {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (z *fakeZot) manifest(w http.ResponseWriter, repo, ref string) {
	key := repo + "@" + ref
	z.mu.Lock()
	z.manifests[key]++
	z.inflight++
	if z.inflight > z.maxFlight {
		z.maxFlight = z.inflight
	}
	gate := z.gate
	z.mu.Unlock()
	defer func() {
		z.mu.Lock()
		z.inflight--
		z.mu.Unlock()
	}()
	if gate != nil {
		<-gate
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if idx, ok := z.indexes[key]; ok {
		raw, _ := idx.RawManifest()
		mt, _ := idx.MediaType()
		w.Header().Set("Content-Type", string(mt))
		_, _ = w.Write(raw)
		return
	}
	img, ok := z.upstream[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if z.blobsToo {
		cfg, _ := img.ConfigName()
		z.blobs[repo+"/"+cfg.String()] = true
		ls, _ := img.Layers()
		for _, l := range ls {
			d, _ := l.Digest()
			z.blobs[repo+"/"+d.String()] = true
		}
	}
	raw, _ := img.RawManifest()
	mt, _ := img.MediaType()
	w.Header().Set("Content-Type", string(mt))
	_, _ = w.Write(raw)
}

type fakeResolver struct {
	mu      sync.Mutex
	digests map[string]string
	fail    map[string]error
}

func (f *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[ref]; err != nil {
		return "", err
	}
	return f.digests[ref], nil
}

func prewarmHandler(t *testing.T, z *fakeZot, res *fakeResolver, cfg PrewarmConfig) *Handler {
	t.Helper()
	cfg.Enabled = true
	cfg.RegistryAddr = z.addr()
	if cfg.Registries == nil {
		cfg.Registries = []string{"docker.io", "ghcr.io", "quay.io"}
	}
	cfg.Resolver = res
	h := NewHandler(nil, nil, nil)
	h.SetPrewarm(cfg)
	return h
}

func waitState(t *testing.T, h *Handler, img string, want protobuf.PrewarmState) *protobuf.PrewarmImageStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{img}})
		if err != nil {
			t.Fatal(err)
		}
		if st := r.Images[0]; st.State == want {
			return st
		} else if time.Now().After(deadline) {
			t.Fatalf("%s: state %v (%s), want %v", img, st.State, st.Error, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrewarmFillsTheCacheWithManifestAndBlobs(t *testing.T) {
	z := newFakeZot(t)
	d := z.addImage(t, "ghcr.io/o/app")
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"ghcr.io/o/app:1": d.String()}}, PrewarmConfig{})

	first, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/app:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := first.Images[0].State; s != protobuf.PrewarmState_PREWARM_STATE_QUEUED && s != protobuf.PrewarmState_PREWARM_STATE_WARMING && s != protobuf.PrewarmState_PREWARM_STATE_DONE {
		t.Fatalf("first answer %v", s)
	}
	st := waitState(t, h, "ghcr.io/o/app:1", protobuf.PrewarmState_PREWARM_STATE_DONE)
	if st.Digest != d.String() || st.Error != "" || st.UpdatedUnixMs == 0 {
		t.Fatalf("status %+v", st)
	}
	img := z.upstream["ghcr.io/o/app@"+d.String()]
	cfg, _ := img.ConfigName()
	if !z.hasBlob("ghcr.io/o/app/" + cfg.String()) {
		t.Fatal("the config blob must be stored")
	}
	ls, _ := img.Layers()
	for _, l := range ls {
		ld, _ := l.Digest()
		if !z.hasBlob("ghcr.io/o/app/" + ld.String()) {
			t.Fatalf("layer %s not stored", ld)
		}
	}
	// A repeated call is an answer, not new work.
	n := z.count("ghcr.io/o/app@" + d.String())
	again, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/app:1"}})
	if again.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_DONE || z.count("ghcr.io/o/app@"+d.String()) != n {
		t.Fatal("a repeated call for a fresh DONE image must not fetch again")
	}
}

func TestPrewarmChecksEveryPlatformOfAnIndex(t *testing.T) {
	z := newFakeZot(t)
	d := z.addIndex(t, "quay.io/o/multi")
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"quay.io/o/multi:2": d.String()}}, PrewarmConfig{})
	if _, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"quay.io/o/multi:2"}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, h, "quay.io/o/multi:2", protobuf.PrewarmState_PREWARM_STATE_DONE)
	idx := z.indexes["quay.io/o/multi@"+d.String()]
	m, _ := idx.IndexManifest()
	for _, c := range m.Manifests {
		if z.count("quay.io/o/multi@"+c.Digest.String()) == 0 {
			t.Fatalf("child manifest %s was never requested", c.Digest)
		}
	}
}

func TestPrewarmFailsWhenOnlyTheManifestWasStored(t *testing.T) {
	z := newFakeZot(t)
	z.blobsToo = false
	d := z.addImage(t, "ghcr.io/o/half")
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"ghcr.io/o/half:1": d.String()}}, PrewarmConfig{})
	if _, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/half:1"}}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, h, "ghcr.io/o/half:1", protobuf.PrewarmState_PREWARM_STATE_FAILED)
	if !strings.Contains(st.Error, "not stored in the cache") {
		t.Fatalf("the error must say that a blob is missing: %q", st.Error)
	}
}

func TestPrewarmSkipsRegistriesTheCacheDoesNotServe(t *testing.T) {
	z := newFakeZot(t)
	h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{})
	r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"registry.example.com/team/app:1", "localhost:5035/lab/x/y/z@sha256:" + strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Images {
		if s.State != protobuf.PrewarmState_PREWARM_STATE_SKIPPED {
			t.Fatalf("%s: %v", s.Image, s.State)
		}
	}
	if z.count("registry.example.com/team/app@")+len(z.manifestsCopy()) != 0 {
		t.Fatal("a skipped image must cause no request")
	}
}

func TestPrewarmReportsErrorsPerImageAndRetriesFailures(t *testing.T) {
	z := newFakeZot(t)
	d := z.addImage(t, "docker.io/library/nginx")
	res := &fakeResolver{digests: map[string]string{"nginx:1": d.String()}, fail: map[string]error{"nginx:1": errors.New("upstream down")}}
	h := prewarmHandler(t, z, res, PrewarmConfig{RetryAfter: 300 * time.Millisecond})
	r, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"nginx:1", "NOT A REF!!"}})
	if r.Images[1].State != protobuf.PrewarmState_PREWARM_STATE_FAILED || !strings.Contains(r.Images[1].Error, "invalid image reference") {
		t.Fatalf("invalid reference: %+v", r.Images[1])
	}
	st := waitState(t, h, "nginx:1", protobuf.PrewarmState_PREWARM_STATE_FAILED)
	if !strings.Contains(st.Error, "upstream down") {
		t.Fatalf("error %q", st.Error)
	}
	// A poller keeps seeing the failure until RetryAfter has passed.
	if again, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"nginx:1"}}); again.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_FAILED {
		t.Fatalf("a fresh failure must be reported, got %v", again.Images[0].State)
	}
	res.mu.Lock()
	delete(res.fail, "nginx:1") // upstream is back: after RetryAfter asking again retries
	res.mu.Unlock()
	waitState(t, h, "nginx:1", protobuf.PrewarmState_PREWARM_STATE_DONE)
}

func TestPrewarmFailsPreconditionWhenTheCacheIsOff(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	if _, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"nginx:1"}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
	h.SetPrewarm(PrewarmConfig{Enabled: false})
	if _, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
}

func TestPrewarmBoundsConcurrency(t *testing.T) {
	z := newFakeZot(t)
	z.gate = make(chan struct{})
	res := &fakeResolver{digests: map[string]string{}}
	var imgs []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		d := z.addImage(t, "ghcr.io/o/"+n)
		res.digests["ghcr.io/o/"+n+":1"] = d.String()
		imgs = append(imgs, "ghcr.io/o/"+n+":1")
	}
	h := prewarmHandler(t, z, res, PrewarmConfig{Concurrency: 2})
	if _, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: imgs}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	r, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{})
	warming, queued := 0, 0
	for _, s := range r.Images {
		switch s.State {
		case protobuf.PrewarmState_PREWARM_STATE_WARMING:
			warming++
		case protobuf.PrewarmState_PREWARM_STATE_QUEUED:
			queued++
		}
	}
	if warming != 2 || queued != 3 {
		t.Fatalf("with concurrency 2 two images warm and three wait: warming=%d queued=%d", warming, queued)
	}
	close(z.gate)
	for _, img := range imgs {
		waitState(t, h, img, protobuf.PrewarmState_PREWARM_STATE_DONE)
	}
	z.mu.Lock()
	maxFlight := z.maxFlight
	z.mu.Unlock()
	if maxFlight > 2 {
		t.Fatalf("at most 2 manifest requests at once, saw %d", maxFlight)
	}
}

func TestPrewarmChecksAStaleDoneImageAgain(t *testing.T) {
	z := newFakeZot(t)
	d := z.addImage(t, "ghcr.io/o/app")
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"ghcr.io/o/app:1": d.String()}}, PrewarmConfig{StaleAfter: time.Hour, Now: clock})
	_, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/app:1"}})
	waitState(t, h, "ghcr.io/o/app:1", protobuf.PrewarmState_PREWARM_STATE_DONE)
	n := z.count("ghcr.io/o/app@" + d.String())

	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	_, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/app:1"}})
	deadline := time.Now().Add(3 * time.Second)
	for z.count("ghcr.io/o/app@"+d.String()) == n {
		if time.Now().After(deadline) {
			t.Fatal("a stale DONE image must be asked of the cache again (it counts as a pull for the retention)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrewarmTimesOutPerImage(t *testing.T) {
	z := newFakeZot(t)
	z.gate = make(chan struct{})
	defer close(z.gate)
	d := z.addImage(t, "ghcr.io/o/slow")
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"ghcr.io/o/slow:1": d.String()}}, PrewarmConfig{Timeout: 200 * time.Millisecond})
	_, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/slow:1"}})
	st := waitState(t, h, "ghcr.io/o/slow:1", protobuf.PrewarmState_PREWARM_STATE_FAILED)
	if !strings.Contains(st.Error, "deadline") && !strings.Contains(st.Error, "context") {
		t.Fatalf("error %q", st.Error)
	}
}

func TestPrewarmEmptyRequestReportsEverythingKnown(t *testing.T) {
	z := newFakeZot(t)
	d := z.addImage(t, "ghcr.io/o/app")
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"ghcr.io/o/app:1": d.String()}}, PrewarmConfig{})
	_, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/o/app:1", "registry.example.com/x:1"}})
	r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{})
	if err != nil || len(r.Images) != 2 {
		t.Fatalf("%v %v", r, err)
	}
}

func tenantPolicyHandler(t *testing.T, z *fakeZot, res *fakeResolver, ten *laboratoryv1alpha1.Tenant, deny []string) *Handler {
	t.Helper()
	h := prewarmHandler(t, z, res, PrewarmConfig{})
	h.cs = fake.NewSimpleClientset(ten)
	h.SetImagePolicy(deny, "localhost:5035")
	return h
}

func TestPrewarmAppliesTheTenantImagePolicy(t *testing.T) {
	z := newFakeZot(t)
	d := z.addImage(t, "ghcr.io/acme/app")
	res := &fakeResolver{digests: map[string]string{"ghcr.io/acme/app:1": d.String()}}
	ten := &laboratoryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       laboratoryv1alpha1.TenantSpec{Images: laboratoryv1alpha1.TenantImages{Allow: []string{"ghcr.io/acme/"}}},
	}
	h := tenantPolicyHandler(t, z, res, ten, []string{"ghcr.io/acme/private"})
	ctx := context.Background()
	r, err := h.PrewarmImages(ctx, &protobuf.PrewarmImagesRequest{Images: []string{
		"ghcr.io/other/app:1", "ghcr.io/acme/private/x:1", "localhost:5035/ghcr.io/acme/app:1", "ghcr.io/acme/app:1"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if r.Images[i].State != protobuf.PrewarmState_PREWARM_STATE_FAILED || r.Images[i].Error == "" {
			t.Errorf("%s must be refused, got %v", r.Images[i].Image, r.Images[i].State)
		}
	}
	if s := r.Images[3].State; s == protobuf.PrewarmState_PREWARM_STATE_FAILED || s == protobuf.PrewarmState_PREWARM_STATE_SKIPPED {
		t.Errorf("the allowed image is fetched, got %v", s)
	}
	waitState(t, h, "ghcr.io/acme/app:1", protobuf.PrewarmState_PREWARM_STATE_DONE)
	if z.manifestFetches("ghcr.io/other/app") != 0 {
		t.Error("a refused image must never reach the cache")
	}

	// The listing of every warmed image shows only what this tenant may use.
	h.cs = fake.NewSimpleClientset(&laboratoryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       laboratoryv1alpha1.TenantSpec{Images: laboratoryv1alpha1.TenantImages{Allow: []string{"quay.io/"}}},
	})
	all, err := h.PrewarmImages(ctx, &protobuf.PrewarmImagesRequest{})
	if err != nil || len(all.Images) != 0 {
		t.Fatalf("a tenant sees only its own images: %+v %v", all, err)
	}
}

func TestPrewarmSkipsTenantsWithTheirOwnCredentials(t *testing.T) {
	z := newFakeZot(t)
	h := tenantPolicyHandler(t, z, &fakeResolver{}, &laboratoryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       laboratoryv1alpha1.TenantSpec{Images: laboratoryv1alpha1.TenantImages{PullSecret: "mine"}},
	}, nil)
	r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/acme/app:1"}})
	if err != nil || r.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_SKIPPED {
		t.Fatalf("%+v %v", r, err)
	}
	if z.manifestFetches("ghcr.io/acme/app") != 0 {
		t.Error("the shared cache must not fetch it")
	}
}
