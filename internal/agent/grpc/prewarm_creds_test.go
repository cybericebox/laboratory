package grpc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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
	if s := got["ghcr.io/acme/missing:1"]; s.State != protobuf.PrewarmState_PREWARM_STATE_FAILED || !strings.Contains(s.Error, "MANIFEST_UNKNOWN") {
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
