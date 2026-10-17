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

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// credResolver is the upstream check of credentialed images: it records the login the
// keychain gives for the image's registry.
type credResolver struct {
	mu     sync.Mutex
	logins map[string]string // ref -> "user:password" the keychain gave
	fail   map[string]error
}

func (c *credResolver) factory(kc authn.Keychain) imagecache.Resolver {
	return resolverFunc(func(_ context.Context, ref string) (string, error) {
		parsed, err := name.ParseReference(ref)
		if err != nil {
			return "", err
		}
		auth, err := kc.Resolve(parsed.Context())
		if err != nil {
			return "", err
		}
		cfg, _ := auth.Authorization()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.logins == nil {
			c.logins = map[string]string{}
		}
		c.logins[ref] = cfg.Username + ":" + cfg.Password
		if err := c.fail[ref]; err != nil {
			return "", err
		}
		return "sha256:" + strings.Repeat("c", 64), nil
	})
}

type resolverFunc func(ctx context.Context, ref string) (string, error)

func (f resolverFunc) Resolve(ctx context.Context, ref string) (string, error) { return f(ctx, ref) }

func creds(host, user, pass string) *protobuf.RegistryCredential {
	return &protobuf.RegistryCredential{Registry: host, Username: user, Password: pass}
}

func TestPrewarmChecksCredentialedImagesWithTheirOwnKeychainAndDoesNotCacheThem(t *testing.T) {
	z := newFakeZot(t)
	d := z.addImage(t, "ghcr.io/o/public")
	cr := &credResolver{fail: map[string]error{"ghcr.io/acme/missing:1": errors.New("MANIFEST_UNKNOWN: not found")}}
	h := prewarmHandler(t, z, &fakeResolver{digests: map[string]string{"ghcr.io/o/public:1": d.String()}},
		PrewarmConfig{CredentialedResolver: cr.factory})

	r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{
		Images: []string{"ghcr.io/acme/app:1", "ghcr.io/acme/missing:1", "ghcr.io/o/public:1", "acme/hub:2", "quay.io/x/y:1"},
		RegistryCredentials: []*protobuf.RegistryCredential{
			creds("ghcr.io", "bob", "tok-1"),
			creds("index.docker.io", "alice", "tok-2"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*protobuf.PrewarmImageStatus{}
	for _, s := range r.Images {
		got[s.Image] = s
	}
	// Every ghcr.io image matches the credential; the shared cache never sees them.
	if s := got["ghcr.io/acme/app:1"]; s.State != protobuf.PrewarmState_PREWARM_STATE_DONE || s.Digest != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("app: %+v", s)
	}
	if s := got["ghcr.io/acme/missing:1"]; s.State != protobuf.PrewarmState_PREWARM_STATE_FAILED || s.Error != "registry unreachable or rejected the credentials" {
		t.Fatalf("missing: %+v", s)
	}
	if s := got["ghcr.io/o/public:1"]; s.State != protobuf.PrewarmState_PREWARM_STATE_DONE {
		t.Fatalf("a matching registry is checked with the credentials too: %+v", s)
	}
	// docker.io and index.docker.io are the same registry.
	if s := got["acme/hub:2"]; s.State != protobuf.PrewarmState_PREWARM_STATE_DONE {
		t.Fatalf("hub: %+v", s)
	}
	if cr.logins["ghcr.io/acme/app:1"] != "bob:tok-1" || cr.logins["acme/hub:2"] != "alice:tok-2" {
		t.Fatalf("logins %v", cr.logins)
	}
	// No credential for quay.io: the normal path (fetched into the cache).
	waitState(t, h, "quay.io/x/y:1", protobuf.PrewarmState_PREWARM_STATE_FAILED)
	if _, ok := cr.logins["quay.io/x/y:1"]; ok {
		t.Fatal("an image without a matching credential is not checked with credentials")
	}
	for _, s := range r.Images {
		if strings.Contains(s.Error, "tok-") {
			t.Fatalf("token in %+v", s)
		}
	}

	// Never stored in the shared entries: a request without credentials starts fresh.
	h.prewarm.mu.Lock()
	_, stored := h.prewarm.entries["ghcr.io/acme/app:1"]
	_, storedHub := h.prewarm.entries["acme/hub:2"]
	h.prewarm.mu.Unlock()
	if stored || storedHub {
		t.Fatal("credentialed images must not enter the shared entries map")
	}
	if z.count("ghcr.io/acme/app@") != 0 {
		t.Fatal("credentialed images are never fetched into the cache")
	}
	again, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/acme/app:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Images[0].State == protobuf.PrewarmState_PREWARM_STATE_DONE && again.Images[0].Digest == "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("the earlier credentialed result leaked to a request without credentials: %+v", again.Images[0])
	}
	if listing, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{}); func() bool {
		for _, s := range listing.Images {
			if s.Image == "acme/hub:2" {
				return true
			}
		}
		return false
	}() {
		t.Fatal("the listing shows a credentialed image")
	}
}

func TestPrewarmRejectsInvalidCredentials(t *testing.T) {
	z := newFakeZot(t)
	h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{})
	_, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{
		Images:              []string{"ghcr.io/a/b:1"},
		RegistryCredentials: []*protobuf.RegistryCredential{creds("ghcr.io", "bob", "")},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
}

func TestPrewarmCredentialedImagesStillFollowTheTenantPolicy(t *testing.T) {
	z := newFakeZot(t)
	cr := &credResolver{}
	h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{CredentialedResolver: cr.factory})
	r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{
		Images:              []string{"localhost:5035/a/b:1"},
		RegistryCredentials: []*protobuf.RegistryCredential{creds("localhost:5035", "bob", "tok-1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_FAILED || len(cr.logins) != 0 {
		t.Fatalf("a refused image is never contacted: %+v %v", r.Images[0], cr.logins)
	}
}

// credRegistry is a registry that wants basic auth bob:tok-1 and knows one tag.
func credRegistry(t *testing.T, known string) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if u, p, ok := r.BasicAuth(); !ok || u != "bob" || p != "tok-1" {
			w.Header().Set("WWW-Authenticate", `Basic realm="r"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			return
		}
		if strings.HasSuffix(r.URL.Path, known) {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("d", 64))
			w.Header().Set("Content-Length", "2")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

type rewriteTransport struct{ to *httptest.Server }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(rt.to.URL, "http://")
	return http.DefaultTransport.RoundTrip(r)
}

// A digest-pinned image is checked against the registry like any other: pinning is no proof
// that the credentials work or that the image exists.
func TestPrewarmCredentialedPinnedImageContactsTheRegistry(t *testing.T) {
	pin := "sha256:" + strings.Repeat("d", 64)
	srv, hits := credRegistry(t, pin)
	z := newFakeZot(t)
	h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{CredTransport: rewriteTransport{srv}})
	req := func(img, pass string) *protobuf.PrewarmImageStatus {
		r, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{
			Images: []string{img}, RegistryCredentials: []*protobuf.RegistryCredential{creds("reg.example.com", "bob", pass)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r.Images[0]
	}
	if s := req("reg.example.com/acme/app@"+pin, "tok-1"); s.State != protobuf.PrewarmState_PREWARM_STATE_DONE || s.Digest != pin {
		t.Fatalf("existing pin: %+v", s)
	}
	before := *hits
	if s := req("reg.example.com/acme/app@sha256:"+strings.Repeat("e", 64), "tok-1"); s.State != protobuf.PrewarmState_PREWARM_STATE_FAILED {
		t.Fatalf("a pin the registry does not have: %+v", s)
	}
	if *hits == before {
		t.Fatal("the registry was not contacted")
	}
	if s := req("reg.example.com/acme/app@"+pin, "wrong"); s.State != protobuf.PrewarmState_PREWARM_STATE_FAILED || strings.Contains(s.Error, "wrong") {
		t.Fatalf("bad credentials: %+v", s)
	}
}

func TestPrewarmCredentialedErrorsAreGeneric(t *testing.T) {
	z := newFakeZot(t)
	cr := &credResolver{fail: map[string]error{"ghcr.io/a/b:1": errors.New("dial tcp 10.0.0.5:443: connect: refused secret-detail")}}
	h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{CredentialedResolver: cr.factory})
	r, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{
		Images: []string{"ghcr.io/a/b:1"}, RegistryCredentials: []*protobuf.RegistryCredential{creds("ghcr.io", "bob", "tok-1")},
	})
	if r.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_FAILED || r.Images[0].Error != "registry unreachable or rejected the credentials" {
		t.Fatalf("%+v", r.Images[0])
	}
}

func TestGuardedTransportRefusesInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	c := &http.Client{Transport: guardedTransport(), Timeout: 2 * time.Second}
	for _, u := range []string{srv.URL, "http://10.1.2.3:80/", "http://169.254.169.254/", "http://0.0.0.0:80/", "http://[::1]:80/"} {
		if resp, err := c.Get(u); err == nil {
			resp.Body.Close()
			t.Fatalf("%s was reached", u)
		} else if !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s: %v", u, err)
		}
	}
}

// blockingResolver waits for its context.
func blockingResolver(_ authn.Keychain) imagecache.Resolver {
	return resolverFunc(func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
}

func TestPrewarmCredentialedChecksAreBounded(t *testing.T) {
	z := newFakeZot(t)
	c := []*protobuf.RegistryCredential{creds("ghcr.io", "bob", "tok-1")}
	t.Run("per image", func(t *testing.T) {
		h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{CredentialedResolver: blockingResolver, CredTimeout: 30 * time.Millisecond, CredTotalTimeout: time.Minute})
		r, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/a/b:1"}, RegistryCredentials: c})
		if s := r.Images[0]; s.State != protobuf.PrewarmState_PREWARM_STATE_FAILED || !strings.Contains(s.Error, "in time") {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("whole call", func(t *testing.T) {
		h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{CredentialedResolver: blockingResolver, Concurrency: 1, CredTimeout: time.Minute, CredTotalTimeout: 30 * time.Millisecond})
		start := time.Now()
		r, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"ghcr.io/a/b:1", "ghcr.io/a/c:1", "ghcr.io/a/d:1"}, RegistryCredentials: c})
		if time.Since(start) > 5*time.Second || len(r.Images) != 3 {
			t.Fatalf("not bounded: %v %d", time.Since(start), len(r.Images))
		}
		for _, s := range r.Images {
			if s.State != protobuf.PrewarmState_PREWARM_STATE_FAILED || !strings.Contains(s.Error, "in time") {
				t.Fatalf("%+v", s)
			}
		}
	})
}

func TestPrewarmKeepsTheRequestOrder(t *testing.T) {
	z := newFakeZot(t)
	cr := &credResolver{}
	h := prewarmHandler(t, z, &fakeResolver{}, PrewarmConfig{CredentialedResolver: cr.factory})
	imgs := []string{"ghcr.io/a/1:1", "quay.io/x/2:1", "ghcr.io/a/3:1", "registry.example.com/x:1", "ghcr.io/a/1:1"}
	r, _ := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: imgs, RegistryCredentials: []*protobuf.RegistryCredential{creds("ghcr.io", "bob", "tok-1")}})
	var got []string
	for _, s := range r.Images {
		got = append(got, s.Image)
	}
	if strings.Join(got, ",") != "ghcr.io/a/1:1,quay.io/x/2:1,ghcr.io/a/3:1,registry.example.com/x:1" {
		t.Fatalf("order %v", got)
	}
}

func TestKeychainHasIgnoresHostCase(t *testing.T) {
	kc, err := imagecache.NewDockerConfigKeychain([]byte(`{"auths":{"GHCR.io":{"username":"u","password":"p"}}}`))
	if err != nil || !kc.Has("ghcr.IO") || !kc.Has("GHCR.io") {
		t.Fatalf("%v", err)
	}
}
