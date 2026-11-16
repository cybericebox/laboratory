package registrygate

import (
	"context"
	"io"
	"testing"
	"time"
)

func obj(repo, digest string) Object {
	return Object{Repo: repo, Kind: "manifests", Ref: digest}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if s != "bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:library/nginx:pull" {
		t.Fatalf("%q %v", s, p)
	}
	if s, p := parseChallenge(`Basic realm="p0-gate"`); s != "basic" || p["realm"] != "p0-gate" {
		t.Fatalf("%q %v", s, p)
	}
	if s, _ := parseChallenge(`Basic`); s != "basic" {
		t.Fatalf("%q", s)
	}
}

func TestCheckPublicPrivateAndWrongCredentials(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"], f.users["mallory"] = "pw", "pw2"
	f.repos["org/private"] = map[string]bool{"alice": true}
	f.public["library/nginx"] = true
	dPub := f.put("library/nginx", "manifests", []byte("pub"))
	dPriv := f.put("org/private", "manifests", []byte("priv"))
	u, reg := NewUpstream(5*time.Second), f.registry("up")
	ctx := context.Background()

	if o, err := u.Check(ctx, reg, obj("library/nginx", dPub), Creds{}, ""); o != Allowed || err != nil {
		t.Fatalf("a public image anonymously: %v %v", o, err)
	}
	if o, _ := u.Check(ctx, reg, obj("org/private", dPriv), Creds{}, ""); o != Denied {
		t.Fatalf("a private image anonymously: %v", o)
	}
	if o, _ := u.Check(ctx, reg, obj("org/private", dPriv), Creds{"alice", "pw"}, "h-alice"); o != Allowed {
		t.Fatalf("alice reads it: %v", o)
	}
	if o, _ := u.Check(ctx, reg, obj("org/private", dPriv), Creds{"mallory", "pw2"}, "h-mallory"); o != Denied {
		t.Fatalf("mallory has an account but no right: %v", o)
	}
	if o, _ := u.Check(ctx, reg, obj("org/private", dPriv), Creds{"alice", "wrong"}, "h-wrong"); o != Denied {
		t.Fatalf("a wrong password is refused by the token endpoint: %v", o)
	}
	if o, _ := u.Check(ctx, reg, obj("org/private", "sha256:00"), Creds{"alice", "pw"}, "h-alice"); o != NotFound {
		t.Fatalf("a 404 on the proof is NotFound; the cache decides what it means: %v", o)
	}
}

// Docker Hub gives a token with an empty scope for a repository one cannot read: the HEAD is the proof, not the token (g7).
func TestAnEmptyScopeTokenIsNoProof(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["mallory"] = "pw"
	f.repos["org/private"] = map[string]bool{}
	f.emptyScope = true
	d := f.put("org/private", "manifests", []byte("priv"))
	o, err := NewUpstream(5*time.Second).Check(context.Background(), f.registry("up"), obj("org/private", d), Creds{"mallory", "pw"}, "h")
	if o != Denied || err != nil {
		t.Fatalf("%v %v", o, err)
	}
}

// An outage is not a denial (429, 5xx, a connection that fails).
func TestUnreachableIsNotADenial(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"] = "pw"
	f.repos["org/private"] = map[string]bool{"alice": true}
	d := f.put("org/private", "manifests", []byte("priv"))
	u, reg := NewUpstream(2*time.Second), f.registry("up")
	for _, code := range []int{429, 500, 502, 503} {
		f.setFail(code)
		if o, _ := u.Check(context.Background(), reg, obj("org/private", d), Creds{"alice", "pw"}, "h"+string(rune(code))); o != Unreachable {
			t.Errorf("%d: %v", code, o)
		}
	}
	f.setFail(0)
	f.srv.Close()
	if o, err := u.Check(context.Background(), reg, obj("org/private", d), Creds{"alice", "pw"}, "hx"); o != Unreachable || err == nil {
		t.Fatalf("a closed server: %v %v", o, err)
	}
}

// One token and one HEAD for a fresh check; the token is reused for the next call of the same credentials and repository (and only those).
func TestTokenIsReusedPerCredentialsAndRepository(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"], f.users["bob"] = "pw", "pw"
	f.repos["org/private"] = map[string]bool{"alice": true, "bob": true}
	d := f.put("org/private", "manifests", []byte("priv"))
	u, reg := NewUpstream(5*time.Second), f.registry("up")
	ctx := context.Background()
	u.Check(ctx, reg, obj("org/private", d), Creds{"alice", "pw"}, "h-alice")
	if f.n("token") != 1 {
		t.Fatalf("tokens %d", f.n("token"))
	}
	u.Check(ctx, reg, obj("org/private", d), Creds{"alice", "pw"}, "h-alice")
	if f.n("token") != 1 {
		t.Fatalf("a second check of the same credentials reuses the token: %d", f.n("token"))
	}
	u.Check(ctx, reg, obj("org/private", d), Creds{"bob", "pw"}, "h-bob")
	if f.n("token") != 2 {
		t.Fatalf("other credentials need their own token: %d", f.n("token"))
	}
}

func TestFetchFollowsACDNRedirectWithoutAuthorization(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"] = "pw"
	f.repos["org/private"] = map[string]bool{"alice": true}
	f.redirect = true
	d := f.put("org/private", "blobs", []byte("layer bytes"))
	u := NewUpstream(5 * time.Second)
	u.HTTP.Transport = f.cdn.Client().Transport // trusts the test CDN's certificate
	resp, o, err := u.Fetch(context.Background(), f.registry("up"), Object{Repo: "org/private", Kind: "blobs", Ref: d}, Creds{"alice", "pw"}, "h")
	if o != Allowed || err != nil || resp == nil {
		t.Fatalf("%v %v", o, err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "layer bytes" {
		t.Fatalf("body %q", b)
	}
	if f.n("cdn") != 1 || len(f.authSeen) != 1 || f.authSeen[0] != "" {
		t.Fatalf("the CDN must be called once and never with Authorization: %d %q", f.n("cdn"), f.authSeen)
	}
}

func TestFetchRefusesARedirectToAnotherHostOverPlainHTTP(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"] = "pw"
	f.repos["org/private"] = map[string]bool{"alice": true}
	f.redirect, f.plainCDN = true, true
	d := f.put("org/private", "blobs", []byte("layer bytes"))
	resp, o, _ := NewUpstream(5*time.Second).Fetch(context.Background(), f.registry("up"), Object{Repo: "org/private", Kind: "blobs", Ref: d}, Creds{"alice", "pw"}, "h")
	if resp != nil || o == Allowed || f.n("cdn") != 0 {
		t.Fatalf("a redirect over plain http to another host must not be followed: %v %d", o, f.n("cdn"))
	}
}

// A redirect is no proof of access: a registry may redirect without looking at who asks, and a 200 of a CDN says nothing about the
// credentials. The proof must be a direct 200; a redirected answer, whatever it ends in, is inconclusive (Unreachable), never Allowed
// and never a denial. A redirect over plain http to another host is not followed at all.
func TestARedirectIsNoProof(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"] = "pw"
	f.repos["org/private"] = map[string]bool{"alice": true}
	d := f.put("org/private", "manifests", []byte("m"))
	f.redirectManifests = true
	u := NewUpstream(5 * time.Second)
	u.HTTP.Transport = f.cdn.Client().Transport
	reg := f.registry("up")
	ctx := context.Background()

	for _, code := range []int{0, 401, 403, 404, 500} {
		f.cdnStatus = code
		if o, _ := u.Check(ctx, reg, obj("org/private", d), Creds{"alice", "pw"}, "h"); o != Unreachable {
			t.Errorf("redirected manifest (the CDN answers %d): inconclusive, got %v", code, o)
		}
	}
	if len(f.authSeen) != 0 {
		t.Fatalf("the gate must not follow a redirect for a proof: %q", f.authSeen)
	}
	f.cdnStatus = 0
	f.plainCDN = true
	f.redirect = true
	u2 := NewUpstream(5 * time.Second)
	if o, _ := u2.Check(ctx, reg, obj("org/private", d), Creds{"alice", "pw"}, "h"); o == Allowed {
		t.Fatalf("a redirect over plain http to another host is not followed: %v", o)
	}
}

// A data transfer (Fetch) after the decision may follow a redirect, without Authorization.
func TestAFetchFollowsARedirectWithoutAuthorization(t *testing.T) {
	f := newFakeUpstream(t)
	f.users["alice"] = "pw"
	f.repos["org/private"] = map[string]bool{"alice": true}
	d := f.put("org/private", "blobs", []byte("layer"))
	f.redirect = true
	u := NewUpstream(5 * time.Second)
	u.HTTP.Transport = f.cdn.Client().Transport
	resp, o, _ := u.Fetch(context.Background(), f.registry("up"), Object{Repo: "org/private", Kind: "blobs", Ref: d}, Creds{"alice", "pw"}, "h")
	if o != Allowed || resp == nil || len(f.authSeen) != 1 || f.authSeen[0] != "" {
		t.Fatalf("%v %q", o, f.authSeen)
	}
	_ = resp.Body.Close()
}
