package registrygate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRegistries(t *testing.T) {
	list, err := ParseRegistries([]byte(`[{"name":"docker.io","url":"https://registry-1.docker.io"},{"name":"reg.example.com","url":"http://reg.svc:5000"}]`))
	if err != nil || len(list) != 2 || list[0].Name != "docker.io" || list[1].URL.Host != "reg.svc:5000" {
		t.Fatalf("%v %+v", err, list)
	}
	for _, bad := range []string{`not json`, `[{"name":"","url":"https://a"}]`, `[{"name":"a","url":"ftp://a"}]`, `[{"name":"a","url":"a"}]`, `[{"name":"a"}]`} {
		if _, err := ParseRegistries([]byte(bad)); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

// The list is replaced when the file changes, and a broken file keeps the list in force.
func TestRegistrySetWatchesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registries.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"name":"docker.io","url":"https://registry-1.docker.io"}]`)
	var set RegistrySet
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 10)
	if err := set.Watch(ctx, path, 10*time.Millisecond, func(err error) { errs <- err }); err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Get("docker.io"); !ok {
		t.Fatal("loaded at once")
	}
	write(`[{"name":"ghcr.io","url":"https://ghcr.io"}]`)
	waitFor(t, func() bool { _, ok := set.Get("ghcr.io"); return ok })
	if _, ok := set.Get("docker.io"); ok {
		t.Fatal("the new list replaces the old one")
	}
	write(`broken`)
	select {
	case <-errs:
	case <-time.After(2 * time.Second):
		t.Fatal("a broken file is reported")
	}
	if _, ok := set.Get("ghcr.io"); !ok {
		t.Fatal("a broken file keeps the list in force")
	}
	if err := (&RegistrySet{}).Watch(ctx, filepath.Join(t.TempDir(), "none"), time.Second, nil); err == nil {
		t.Fatal("a file that is not there at the start is an error")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// Review finding 9: the credentials of a pull go only to https token endpoints on the registry's own host or one its entry names.
func TestTokenRealmMustBelongToTheRegistry(t *testing.T) {
	hub, _ := ParseRegistries([]byte(`[{"name":"docker.io","url":"https://registry-1.docker.io"},{"name":"ghcr.io","url":"https://ghcr.io"},{"name":"in","url":"http://reg.svc:5000"},{"name":"x","url":"https://x.example","authHosts":["auth.x.example"]}]`))
	by := map[string]Registry{}
	for _, r := range hub {
		by[r.Name] = r
	}
	for _, c := range []struct {
		reg, realm string
		want       bool
	}{
		{"docker.io", "https://auth.docker.io/token", true},
		{"docker.io", "https://evil.example/token", false},
		{"docker.io", "http://auth.docker.io/token", false},
		{"ghcr.io", "https://ghcr.io/token", true},
		{"ghcr.io", "https://auth.docker.io/token", false},
		{"ghcr.io", "http://ghcr.io/token", false},
		{"in", "http://reg.svc:5000/token", true},
		{"in", "http://other.svc:5000/token", false},
		{"in", "https://evil.example/token", false},
		{"x", "https://auth.x.example/t", true},
		{"x", "https://AUTH.X.EXAMPLE/t", true},
		{"x", "ftp://x.example/t", false},
		{"x", "https:///t", false},
	} {
		u, _ := url.Parse(c.realm)
		if got := by[c.reg].realmAllowed(u); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.reg, c.realm, got, c.want)
		}
	}
}

func TestACredentialIsNeverSentToAForeignRealm(t *testing.T) {
	var evilHits int
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilHits++ }))
	defer evil.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+evil.URL+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()
	u := mustURL(t, up.URL)
	_, out, _ := NewUpstream(5*time.Second).Fetch(context.Background(), Registry{Name: "up", URL: u}, Object{Repo: "org/x", Kind: kindManifests, Ref: "latest"}, Creds{"alice", "pw"}, "k")
	if evilHits != 0 {
		t.Fatalf("the foreign token endpoint got %d requests", evilHits)
	}
	if out == Allowed {
		t.Fatal("nothing was proved")
	}
}
