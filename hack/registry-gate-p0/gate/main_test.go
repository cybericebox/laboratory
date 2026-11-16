package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testServer(t *testing.T) *server {
	t.Helper()
	blobs, digest, err := buildImage([]byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		creds:  credFlags{{"up", "t1~alice", "pw"}, {"other", "bob", "pw2"}},
		blobs:  blobs,
		digest: digest,
		out:    out,
	}
}

func get(s *server, method, path, user, pass string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestGateChallengesAndChecksCredentialsPerRegistry(t *testing.T) {
	s := testServer(t)
	manifest := "/v2/c/up/img/manifests/" + s.digest

	if w := get(s, http.MethodGet, "/v2/", "", ""); w.Code != http.StatusOK {
		t.Fatalf("ping must be anonymous: %d", w.Code)
	}
	w := get(s, http.MethodHead, manifest, "", "")
	if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("no credentials: want 401 with a Basic challenge, got %d %v", w.Code, w.Header())
	}
	if w := get(s, http.MethodGet, manifest, "t1~alice", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := get(s, http.MethodGet, "/v2/c/other/img/manifests/"+s.digest, "t1~alice", "pw"); w.Code != http.StatusUnauthorized {
		t.Fatalf("credentials of another registry: %d", w.Code)
	}
	w = get(s, http.MethodGet, manifest, "t1~alice", "pw")
	if w.Code != http.StatusOK || w.Header().Get("Docker-Content-Digest") != s.digest || w.Body.Len() == 0 {
		t.Fatalf("valid credentials: %d %v", w.Code, w.Header())
	}
	if w := get(s, http.MethodHead, manifest, "t1~alice", "pw"); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD has no body: %d %d", w.Code, w.Body.Len())
	}
	if w := get(s, http.MethodGet, "/v2/c/up/img/blobs/sha256:00", "t1~alice", "pw"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown blob: %d", w.Code)
	}
}

func TestGateLogsWhatTheClientSent(t *testing.T) {
	s := testServer(t)
	get(s, http.MethodGet, "/v2/c/up/img/manifests/"+s.digest, "", "")
	get(s, http.MethodGet, "/v2/c/up/img/manifests/"+s.digest, "t1~alice", "pw")
	b, err := os.ReadFile(s.out.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"auth":"none"`, `"status":401`, `"auth":"basic"`, `"user":"t1~alice"`, `"kind":"manifest"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log lacks %s:\n%s", want, b)
		}
	}
}
