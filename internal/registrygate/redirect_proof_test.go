package registrygate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// registry.k8s.io answers every manifest HEAD with a 307. A redirect answer to the manifest HEAD (one the registry gave to a request that
// carried the pull's credentials) counts as the manifest existing; the target is never requested.
func TestAManifestRedirectIsAProofAndTheTargetIsNotRequested(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var targetHits atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", target.URL+"/m")
			w.WriteHeader(code)
		}))
		u := NewUpstream(2 * time.Second)
		o, _, err := u.Proof(context.Background(), Registry{Name: "k8s.test", URL: mustURL(t, reg.URL)}, Object{Repo: "pause", Kind: kindManifests, Ref: "sha256:00"}, Creds{}, "")
		reg.Close()
		target.Close()
		if o != Allowed || err != nil {
			t.Errorf("%d: %v %v, want Allowed", code, o, err)
		}
		if n := targetHits.Load(); n != 0 {
			t.Errorf("%d: the redirect target was requested %d times", code, n)
		}
	}
}

// The same answers as before for everything else: 404 is unavailable, 401 and 403 are refusals, and a redirect of a BLOB head is no proof.
func TestOtherManifestAnswersAndBlobRedirectsAreUnchanged(t *testing.T) {
	status := func(code int, kind string) Outcome {
		reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://cdn.example.test/x")
			w.WriteHeader(code)
		}))
		defer reg.Close()
		o, _, _ := NewUpstream(2*time.Second).Proof(context.Background(), Registry{Name: "up", URL: mustURL(t, reg.URL)}, Object{Repo: "o/r", Kind: kind, Ref: "sha256:00"}, Creds{}, "")
		return o
	}
	for code, want := range map[int]Outcome{200: Allowed, 404: NotFound, 401: Denied, 403: Denied, 500: Unreachable, 429: Unreachable, 304: Unreachable} {
		if got := status(code, kindManifests); got != want {
			t.Errorf("manifest %d: %v, want %v", code, got, want)
		}
	}
	if got := status(http.StatusTemporaryRedirect, kindBlobs); got != Unreachable {
		t.Errorf("a blob redirect: %v, want inconclusive", got)
	}
}
