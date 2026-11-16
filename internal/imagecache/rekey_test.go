package imagecache

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func doc(t *testing.T, entries map[string][2]string) []byte {
	t.Helper()
	auths := map[string]map[string]string{}
	for host, up := range entries {
		auths[host] = map[string]string{"username": up[0], "password": up[1], "auth": base64.StdEncoding.EncodeToString([]byte(up[0] + ":" + up[1]))}
	}
	raw, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parse(t *testing.T, raw []byte) map[string][2]string {
	t.Helper()
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for k, v := range cfg.Auths {
		dec, _ := base64.StdEncoding.DecodeString(v.Auth)
		if string(dec) != v.Username+":"+v.Password {
			t.Errorf("%s: the auth field does not match the username and password", k)
		}
		out[k] = [2]string{v.Username, v.Password}
	}
	return out
}

// The pull Secret of a lab on the cache: one key per registry under the cache path, the tenant in the username, the tenant's credentials
// replaced by the lab's own for the same registry, a registry the cache does not serve dropped.
func TestRekeyForCache(t *testing.T) {
	tenantDoc := doc(t, map[string][2]string{"ghcr.io": {"tenant-bot", "tp"}, "https://index.docker.io/v1/": {"hubuser", "hp"}})
	labDoc := doc(t, map[string][2]string{"GHCR.io": {"lab-bot", "lp"}, "registry.example.com": {"x", "y"}, "quay.io": {"", "nouser"}})
	raw, n, err := RekeyForCache("localhost:5035", "t1", []string{"docker.io", "ghcr.io", "quay.io"}, tenantDoc, labDoc)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	got := parse(t, raw)
	want := map[string][2]string{
		"localhost:5035/c/ghcr.io":   {"t1~lab-bot", "lp"},
		"localhost:5035/c/docker.io": {"t1~hubuser", "hp"},
	}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	// the same input gives the same bytes: the Secret does not change under the kubelet for nothing
	again, _, _ := RekeyForCache("localhost:5035", "t1", []string{"docker.io", "ghcr.io", "quay.io"}, tenantDoc, labDoc)
	if string(again) != string(raw) {
		t.Error("the document must be reproducible")
	}
}

func TestRekeyForCacheNothingToKeep(t *testing.T) {
	raw, n, err := RekeyForCache("localhost:5035", "t1", []string{"ghcr.io"}, doc(t, map[string][2]string{"registry.example.com": {"u", "p"}}))
	if raw != nil || n != 0 || err != nil {
		t.Fatalf("%s %d %v", raw, n, err)
	}
	for _, bad := range []string{"", "a~b", "a:b"} {
		if _, _, err := RekeyForCache("localhost:5035", bad, []string{"ghcr.io"}); err == nil {
			t.Errorf("tenant %q must be refused", bad)
		}
	}
	if _, _, err := RekeyForCache("localhost:5035", "t1", nil, []byte("{")); err == nil {
		t.Error("a malformed document is an error")
	}
}
