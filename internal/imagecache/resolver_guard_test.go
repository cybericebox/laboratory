package imagecache

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/cybericebox/laboratory/internal/netguard"
	"github.com/cybericebox/laboratory/internal/netguard/netguardtest"
)

// manifestRegistry is a registry on the loopback (plain http) whose manifest answers come from manifest; /v2/ answers 200 or, with
// challenge, a Basic challenge for a request that has no Authorization.
func manifestRegistry(t *testing.T, challenge bool, manifest func(w http.ResponseWriter, r *http.Request)) (ref string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if challenge && r.Header.Get("Authorization") == "" {
			w.Header().Set("Www-Authenticate", `Basic realm="r"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			return
		}
		manifest(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://") + "/org/img:tag"
}

func answerWith(digest string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", "2")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("{}"))
		}
	}
}

func redirectTo(loc string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
}

// A registry the reference names is asked wherever it is (here the loopback): only a redirect hop is guarded.
func TestResolverAsksARegistryOnTheLoopbackDirectly(t *testing.T) {
	ref := manifestRegistry(t, false, answerWith(dg1))
	d, err := (&RegistryResolver{TTL: time.Minute}).Resolve(context.Background(), ref)
	if err != nil || d != dg1 {
		t.Fatalf("%s %v", d, err)
	}
}

func hopTarget(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Value) {
	t.Helper()
	var hits atomic.Int64
	var auth atomic.Value
	auth.Store("")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		answerWith(dg2)(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &auth
}

// A redirect to an internal endpoint is not requested: to an IP address, and to a name that resolves to the loopback (DNS rebinding).
func TestResolverDoesNotFollowARedirectToAnInternalEndpoint(t *testing.T) {
	target, hits, _ := hopTarget(t)
	for name, c := range map[string]struct {
		loc string
		rt  http.RoundTripper // nil: the default transport
	}{
		"an IP address":       {target.URL + "/m", nil},
		"the default port":    {"https://127.0.0.1/m", nil},
		"the metadata name":   {"https://169.254.169.254/latest", nil},
		"plain http":          {"http://cdn.example.com/m", nil},
		"a port":              {"https://cdn.example.com:8443/m", nil},
		"userinfo":            {"https://u:p@cdn.example.com/m", nil},
		"a name that rebinds": {"https://rebind.test/m", netguard.Hops{Base: netguard.NewTransport(2*time.Second, netguardtest.DNSServer(t)), MaxHops: 3}},
	} {
		ref := manifestRegistry(t, false, redirectTo(c.loc))
		d, err := (&RegistryResolver{TTL: time.Minute, Transport: c.rt}).Resolve(context.Background(), ref)
		if err == nil || d != "" || hits.Load() != 0 {
			t.Errorf("%s: %q %v, hits %d", name, d, err, hits.Load())
			continue
		}
		// (go-containerregistry itself refuses a redirect to a link-local address, with an error of its own that we cannot classify)
		if pe, ok := err.(*PinError); !ok || (!strings.Contains(pe.Reason, "not allowed") && name != "the metadata name") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A redirect to a public name is followed (the test dials that name to the target), and the credentials of the registry never reach it.
func TestResolverFollowsAPublicRedirectWithoutCredentials(t *testing.T) {
	target, hits, auth := hopTarget(t)
	tr := target.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		if _, port, _ := net.SplitHostPort(addr); port == "443" {
			addr = target.Listener.Addr().String()
		}
		return d.DialContext(ctx, network, addr)
	}
	ref := manifestRegistry(t, true, redirectTo("https://example.com/m"))
	kc, err := NewDockerConfigKeychain([]byte(`{"auths":{"` + strings.SplitN(ref, "/", 2)[0] + `":{"username":"u","password":"p"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := &RegistryResolver{TTL: time.Minute, Keychain: kc, Transport: netguard.Hops{Base: tr, MaxHops: 3}}
	d, err := r.Resolve(context.Background(), ref)
	if err != nil || d != dg2 || hits.Load() != 1 {
		t.Fatalf("%q %v hits %d", d, err, hits.Load())
	}
	if a := auth.Load().(string); a != "" {
		t.Fatalf("the registry's credentials reached the redirect target: %q", a)
	}
}

// What a registry answers is not what an operator reads: the body of an error answer stays out of the error.
func TestResolverErrorCarriesNoBodyOfTheRegistry(t *testing.T) {
	ref := manifestRegistry(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(secretBody))
	})
	r := &RegistryResolver{TTL: time.Minute, Platforms: func(context.Context) []v1.Platform {
		return []v1.Platform{{OS: "linux", Architecture: "amd64"}} // a platform makes it a GET, which carries a body
	}}
	_, err := r.Resolve(context.Background(), ref)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "abc123") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("%v", err)
	}
	if got := SafeError(err); got != err.Error() {
		t.Fatalf("%q", got)
	}
}
