package registrygate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	sch   *Schedule
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
	// the callers: the agent (the prewarm path, credentials of the call) is the one the registry tests use; nodes have their own tests
	sch := NewSchedule()
	sch.Confirm()
	cache.Schedule, cache.Creds, cache.Grants, cache.AgentMaxFills = sch, NewSecretView(), &Grants{}, 128
	g.Cache = cache
	g.Auth = &Authenticator{Reviewer: testReviewer{}, Accounts: testAccounts, Schedule: sch}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return &rig{t: t, up: f, zot: z, cache: cache, gate: srv, clock: clk, sch: sch}
}

// pull is a request of the agent: the path has no tenant segment (the helper adds the one of user, "<tenant>~<upstream user>", or "t0" for
// an anonymous request), the credentials go in a Basic header, the token of the agent in X-Cib-Node-Token. It returns the status and body.
func (r *rig) pull(method, path, user, pass string, hdr map[string]string) (int, []byte, http.Header) {
	r.t.Helper()
	seg, up := "t0", ""
	if user != "" {
		seg, up, _ = strings.Cut(user, "~")
	}
	path = strings.Replace(path, "/v2/c/", "/v2/c/"+seg+"/", 1)
	req, _ := http.NewRequest(method, r.gate.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(HeaderNodeToken, "agent-token")
	if up != "" {
		req.SetBasicAuth(up, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
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

// The callers of the gate in the tests: the three accounts, and a reviewer that knows a few tokens.
var testAccounts = Accounts{
	Node:     Account{"laboratory-system", "laboratory-node-agent"},
	Operator: Account{"laboratory-system", "laboratory-controller-manager"},
	Agent:    Account{"laboratory-agent", "laboratory-agent"},
}

type testReviewer struct{}

func (testReviewer) Review(_ context.Context, token string, aud []string) (Review, error) {
	switch {
	case token == "agent-token":
		return Review{Authenticated: true, Username: "system:serviceaccount:laboratory-agent:laboratory-agent", Audiences: aud}, nil
	case token == "operator-token":
		return Review{Authenticated: true, Username: "system:serviceaccount:laboratory-system:laboratory-controller-manager", Audiences: aud}, nil
	case token == "other-token": // a valid token of an account the gate does not serve
		return Review{Authenticated: true, Username: "system:serviceaccount:default:somebody", Audiences: aud}, nil
	case strings.HasPrefix(token, "node-token-"): // node-token-<pod name>-<pod uid>
		name, uid, _ := strings.Cut(strings.TrimPrefix(token, "node-token-"), "-")
		return Review{Authenticated: true, Username: "system:serviceaccount:laboratory-system:laboratory-node-agent", Audiences: aud,
			Extra: map[string][]string{extraPodName: {name}, extraPodUID: {uid}}}, nil
	}
	return Review{Authenticated: false}, nil
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
