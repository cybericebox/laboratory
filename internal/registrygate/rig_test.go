package registrygate

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// rig is the whole gate in a test: a real zot (the binary named by ZOT_BIN; the tests are skipped without it), a fake upstream registry
// with a Bearer token endpoint, and the gate in front of zot, reached over HTTP like the relay of a node reaches it.
type rig struct {
	t     *testing.T
	up    *fakeUpstream
	zot   *Zot
	cache *Cache
	gate  *httptest.Server
	clock *clock
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startZot(t *testing.T) (addr string) {
	t.Helper()
	bin := os.Getenv("ZOT_BIN")
	if bin == "" {
		t.Skip("ZOT_BIN is not set: the tests with a real zot are skipped")
	}
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("gatepw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte("gate:"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	all := []string{"read", "create", "update", "delete"}
	cfg := map[string]any{
		"distSpecVersion": "1.1.0",
		"storage":         map[string]any{"rootDirectory": filepath.Join(dir, "data"), "dedupe": true, "gc": true},
		"http": map[string]any{
			"address": "127.0.0.1", "port": fmt.Sprint(port), "compat": []string{"docker2s2"},
			"auth": map[string]any{"htpasswd": map[string]any{"path": filepath.Join(dir, "htpasswd")}},
			"accessControl": map[string]any{"repositories": map[string]any{
				"c/**": map[string]any{"policies": []any{map[string]any{"users": []string{"gate"}, "actions": all}}},
			}},
		},
		"log": map[string]any{"level": "error"},
	}
	data, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", cfgPath)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("zot did not start")
	return ""
}

func newRig(t *testing.T) *rig {
	t.Helper()
	addr := startZot(t)
	f := newFakeUpstream(t)
	f.users["alice"], f.users["carol"], f.users["mallory"] = "pw", "pw", "pw"
	f.repos["org/private"] = map[string]bool{"alice": true, "carol": true}
	f.public["library/nginx"] = true
	regs := &RegistrySet{}
	regs.Set([]Registry{f.registry("up")})
	up := NewUpstream(5 * time.Second)
	up.HTTP.Transport = f.cdn.Client().Transport // trusts the test CDN
	z := NewZot(addr, "gate", "gatepw")
	store := NewStore(DefaultTiming)
	clk := &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store.Now = clk.now
	cache := NewCache(regs, []byte("test key"), up, z, store, nil)
	g, err := New(addr)
	if err != nil {
		t.Fatal(err)
	}
	g.Cache = cache
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return &rig{t: t, up: f, zot: z, cache: cache, gate: srv, clock: clk}
}

// pull does what containerd does: a request without credentials, and, after a 401 with a Basic challenge, the same request with the
// credentials of the pod (user is "<tenant>~<upstream user>"). It returns the final status and body.
func (r *rig) pull(method, path, user, pass string, hdr map[string]string) (int, []byte, http.Header) {
	r.t.Helper()
	do := func(withAuth bool) (*http.Response, []byte) {
		req, _ := http.NewRequest(method, r.gate.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if withAuth {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			r.t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, body := do(false)
	if resp.StatusCode == http.StatusUnauthorized && user != "" && strings.HasPrefix(resp.Header.Get("Www-Authenticate"), "Basic") {
		resp, body = do(true)
	}
	return resp.StatusCode, body, resp.Header
}

func manifestBody(config string, layers ...string) []byte {
	type d struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	m := map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": d{"application/vnd.oci.image.config.v1+json", config, 2}}
	var ls []d
	for _, l := range layers {
		ls = append(ls, d{"application/vnd.oci.image.layer.v1.tar+gzip", l, 10})
	}
	m["layers"] = ls
	b, _ := json.Marshal(m)
	return b
}

func (r *rig) inZot(repo, kind, ref string) bool {
	r.t.Helper()
	_, ok, err := r.zot.Head(r.t.Context(), repo, kind, ref)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}
