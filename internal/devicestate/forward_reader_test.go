package devicestate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The reader account is added to the reads of the snapshot repositories and the base repository, and to nothing else.
func TestReaderHandlerAddsTheReaderOnlyWhereItBelongs(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	h := readerHandler(strings.TrimPrefix(up.URL, "http://"), forwardOptions{readerUser: "reader", readerPassword: "pw"})

	do := func(method, path, auth string) string {
		req := httptest.NewRequest(method, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || gotPath != path || gotMethod != method {
			t.Fatalf("%s %s relayed as %s %s: %d", method, path, gotMethod, gotPath, rec.Code)
		}
		return gotAuth
	}
	reader := "Basic cmVhZGVyOnB3" // reader:pw
	for _, p := range []string{"/v2/lab/ns/lab/dev/manifests/sha256:abc", "/v2/lab/ns/lab/dev/blobs/sha256:abc", "/v2/base/manifests/latest", "/v2/base/blobs/sha256:x"} {
		for _, m := range []string{"GET", "HEAD"} {
			if got := do(m, p, ""); got != reader {
				t.Errorf("%s %s: Authorization = %q, want the reader", m, p, got)
			}
		}
	}
	// the public image cache stays anonymous
	for _, p := range []string{"/v2/docker.io/library/nginx/manifests/1", "/v2/ghcr.io/o/app/blobs/sha256:a", "/v2/", "/v2/_catalog"} {
		if got := do("GET", p, ""); got != "" {
			t.Errorf("%s must not get credentials: %q", p, got)
		}
	}
	// writes never get the reader, and credentials the caller brought are left alone (the snapshot pusher is the writer)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if got := do(m, "/v2/lab/ns/lab/dev/blobs/uploads/", ""); got != "" {
			t.Errorf("%s must not get the reader: %q", m, got)
		}
	}
	if got := do("GET", "/v2/lab/ns/lab/dev/manifests/x", "Basic d3JpdGVyOnc="); got != "Basic d3JpdGVyOnc=" {
		t.Errorf("the caller's own credentials stay: %q", got)
	}
	// a path that only starts like a snapshot repository is not one
	if got := do("GET", "/v2/labx/y/manifests/1", ""); got != "" {
		t.Errorf("labx is not lab: %q", got)
	}
}

// The reader goes only to requests that carry the exact Host the runtime pulls from, and never to paths that climb.
func TestReaderHandlerNeedsTheExactHost(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotAuth = r.Header.Get("Authorization") }))
	defer up.Close()
	h := readerHandler(strings.TrimPrefix(up.URL, "http://"), forwardOptions{readerUser: "reader", readerPassword: "pw", host: "localhost:5035"})
	for host, want := range map[string]bool{"localhost:5035": true, "LOCALHOST:5035": true, "127.0.0.1:5035": false, "localhost:5036": false, "evil.example": false, "localhost": false} {
		gotAuth = ""
		req := httptest.NewRequest("GET", "/v2/lab/ns/lab/dev/manifests/x", nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
		if (gotAuth != "") != want {
			t.Errorf("Host %q: reader added = %v, want %v", host, gotAuth != "", want)
		}
	}
	for _, p := range []string{"/v2/lab/../docker.io/x/manifests/1", "/v2/lab//x"} {
		gotAuth = ""
		req := httptest.NewRequest("GET", p, nil)
		req.Host = "localhost:5035"
		h.ServeHTTP(httptest.NewRecorder(), req)
		if gotAuth != "" {
			t.Errorf("%s must not get the reader", p)
		}
	}
}
