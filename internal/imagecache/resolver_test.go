package imagecache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

const dg1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const dg2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestResolverPinsForTTLThenFollowsTheTag(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	answer, calls := dg1, 0
	r := &RegistryResolver{TTL: 10 * time.Minute, Now: func() time.Time { return now },
		head: func(_ context.Context, ref name.Reference, _ authn.Keychain) (string, error) {
			calls++
			if ref.Name() != "index.docker.io/library/nginx:1.25" {
				t.Errorf("resolved %s", ref.Name())
			}
			return answer, nil
		}}
	ctx := context.Background()
	if d, err := r.Resolve(ctx, "nginx:1.25"); err != nil || d != dg1 {
		t.Fatalf("%s %v", d, err)
	}
	answer = dg2 // the upstream tag moves
	now = now.Add(5 * time.Minute)
	if d, _ := r.Resolve(ctx, "docker.io/library/nginx:1.25"); d != dg1 || calls != 1 {
		t.Fatalf("labs of one wave must see one digest: %s after %d calls", d, calls)
	}
	now = now.Add(6 * time.Minute)
	if d, _ := r.Resolve(ctx, "nginx:1.25"); d != dg2 || calls != 2 {
		t.Fatalf("after the TTL the new digest is taken: %s", d)
	}
}

func TestResolverDigestRefsNeedNoRequestAndErrorsAreNotCached(t *testing.T) {
	fail := true
	calls := 0
	r := &RegistryResolver{TTL: time.Hour, head: func(context.Context, name.Reference, authn.Keychain) (string, error) {
		calls++
		if fail {
			return "", errors.New("upstream down")
		}
		return dg1, nil
	}}
	ctx := context.Background()
	if d, err := r.Resolve(ctx, "nginx@"+dg2); err != nil || d != dg2 || calls != 0 {
		t.Fatalf("a digest reference is already pinned: %s %v %d", d, err, calls)
	}
	if _, err := r.Resolve(ctx, "nginx:1"); err == nil {
		t.Fatal("expected the upstream error")
	}
	fail = false
	if d, err := r.Resolve(ctx, "nginx:1"); err != nil || d != dg1 {
		t.Fatalf("a failure must not be remembered: %s %v", d, err)
	}
	if _, err := r.Resolve(ctx, "NOT A REF!!"); err == nil {
		t.Fatal("a malformed reference is an error")
	}
}

func TestDockerConfigKeychain(t *testing.T) {
	doc := []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"ZGg6cDpxcg=="},"ghcr.io":{"username":"gh","password":"tok"}}}`)
	k, err := NewDockerConfigKeychain(doc)
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"nginx:1": "dh", "ghcr.io/o/a:1": "gh", "quay.io/x/y:1": ""} {
		parsed, _ := name.ParseReference(ref)
		a, err := k.Resolve(parsed.Context())
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := a.Authorization()
		if cfg.Username != want {
			t.Errorf("%s: user %q want %q", ref, cfg.Username, want)
		}
	}
	if _, err := NewDockerConfigKeychain([]byte("{")); err == nil {
		t.Fatal("malformed config must fail")
	}
}
