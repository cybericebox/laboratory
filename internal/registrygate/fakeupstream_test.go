package registrygate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeUpstream is a registry with the parts of the protocol the gate meets: public and private repositories, a Bearer token endpoint
// that checks Basic credentials, Docker Hub's empty-scope token, a CDN redirect for blobs, failures on demand, and a count of every call.
type fakeUpstream struct {
	t   *testing.T
	srv *httptest.Server
	cdn *httptest.Server // https, like a real CDN
	// cdnPlain is a plain-http one, to see a redirect that must be refused.
	cdnPlain *httptest.Server

	mu      sync.Mutex
	users   map[string]string          // user -> password
	repos   map[string]map[string]bool // private repository -> users that may read it
	public  map[string]bool
	objects map[string][]byte // path under /v2 -> body
	fail    int               // status to answer instead, 0 = none
	// emptyScope: the token endpoint gives a token to anybody for any repository, as Docker Hub does for one that cannot be read.
	emptyScope bool
	redirect   bool // blobs answer 307 to the CDN
	plainCDN   bool // ... to the plain-http one
	// redirectManifests: manifests redirect to the CDN too, whoever asks (a registry that redirects without looking at who asks).
	redirectManifests bool
	noDigestHeader    bool // manifests are answered without Docker-Content-Digest
	headStatus        int  // when set, HEADs answer this (a registry that hides a repository from a caller who lost access)
	redirectHeads     bool // blob HEADs redirect too (not only the GET)
	cdnStatus         int  // when set, the CDN answers this instead of the object
	calls             map[string]*atomic.Int64
	authSeen          []string // Authorization header of every request to the CDN
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{t: t, users: map[string]string{}, repos: map[string]map[string]bool{}, public: map[string]bool{},
		objects: map[string][]byte{}, calls: map[string]*atomic.Int64{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	cdn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		body := f.objects[strings.TrimPrefix(r.URL.Path, "/cdn")]
		status := f.cdnStatus
		f.mu.Unlock()
		f.count("cdn")
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	})
	f.cdn, f.cdnPlain = httptest.NewTLSServer(cdn), httptest.NewServer(cdn)
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.cdn.Close)
	t.Cleanup(f.cdnPlain.Close)
	return f
}

func (f *fakeUpstream) registry(name string) Registry {
	u, _ := url.Parse(f.srv.URL)
	return Registry{Name: name, URL: u}
}

func (f *fakeUpstream) count(k string) {
	f.mu.Lock()
	c := f.calls[k]
	if c == nil {
		c = &atomic.Int64{}
		f.calls[k] = c
	}
	f.mu.Unlock()
	c.Add(1)
}

func (f *fakeUpstream) n(k string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.calls[k]; c != nil {
		return c.Load()
	}
	return 0
}

// put stores a blob or a manifest in a repository and returns its digest.
func (f *fakeUpstream) put(repo, kind string, body []byte) string {
	sum := sha256.Sum256(body)
	d := "sha256:" + hex.EncodeToString(sum[:])
	f.mu.Lock()
	f.objects["/v2/"+repo+"/"+kind+"/"+d] = body
	f.mu.Unlock()
	return d
}

func (f *fakeUpstream) setFail(code int) { f.mu.Lock(); f.fail = code; f.mu.Unlock() }

func repoOf(path string) string {
	rest := strings.TrimPrefix(path, "/v2/")
	for _, k := range []string{"/manifests/", "/blobs/"} {
		if i := strings.Index(rest, k); i >= 0 {
			return rest[:i]
		}
	}
	return ""
}

func (f *fakeUpstream) mayRead(repo, user, pass string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.public[repo] {
		return true
	}
	if p, ok := f.users[user]; !ok || p != pass {
		return false
	}
	return f.repos[repo][user]
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if r.URL.Path == "/token" {
		f.count("token")
		user, pass, _ := r.BasicAuth()
		scope := strings.TrimSuffix(strings.TrimPrefix(r.URL.Query().Get("scope"), "repository:"), ":pull")
		f.mu.Lock()
		okUser := user == "" || f.users[user] == pass
		empty := f.emptyScope
		f.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(fail)
			return
		}
		if !okUser {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		tok := "anon"
		if f.mayRead(scope, user, pass) || empty {
			tok = fmt.Sprintf("tok:%s:%s:%s", user, pass, scope)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_in": 300})
		return
	}
	repo := repoOf(r.URL.Path)
	f.count(r.Method + " " + repo)
	if fail != 0 {
		w.WriteHeader(fail)
		return
	}
	f.mu.Lock()
	redirectManifest := f.redirectManifests
	f.mu.Unlock()
	if redirectManifest && strings.Contains(r.URL.Path, "/manifests/") {
		target := f.cdn.URL
		if f.plainCDN {
			target = f.cdnPlain.URL
		}
		w.Header().Set("Location", target+"/cdn"+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	ok := false
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer tok:") {
		parts := strings.SplitN(strings.TrimPrefix(auth, "Bearer tok:"), ":", 3)
		ok = len(parts) == 3 && parts[2] == repo && f.mayRead(repo, parts[0], parts[1])
		if !ok && !f.mayRead(repo, "", "") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if !ok && f.mayRead(repo, "", "") {
		ok = true
	}
	if !ok {
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, f.srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	body, found := f.objects[r.URL.Path]
	redirect, redirectHeads, headStatus, noDigest := f.redirect, f.redirectHeads, f.headStatus, f.noDigestHeader
	f.mu.Unlock()
	if headStatus != 0 && r.Method == http.MethodHead {
		w.WriteHeader(headStatus)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if redirect && strings.Contains(r.URL.Path, "/blobs/") && (r.Method == http.MethodGet || redirectHeads) {
		target := f.cdn.URL
		if f.plainCDN {
			target = f.cdnPlain.URL
		}
		w.Header().Set("Location", target+"/cdn"+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("Content-Type", "application/octet-stream")
	if strings.Contains(r.URL.Path, "/manifests/") && !noDigest {
		sum := sha256.Sum256(body)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
