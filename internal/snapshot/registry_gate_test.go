package snapshot

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// The client has no credentials and touches no shared repository: it mounts the layers of the base image from the cache repository
// (the gate allows that source) into its own device repository, and every request goes where the transport sends it.
func TestPushMountsTheBaseLayersFromTheCacheRepositoryAndNeverTouchesBase(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	reg := &Registry{Host: strings.TrimPrefix(srv.URL, "http://")}
	ctx := context.Background()

	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	const cache = "c/docker.io/library/app"
	if _, _, err := reg.Push(ctx, cache, img, 0, ""); err != nil {
		t.Fatal(err)
	}
	layers, _ := img.Layers()
	mu.Lock()
	paths, auth = nil, nil
	mu.Unlock()
	if _, _, err := reg.Push(ctx, Repo("ns", "lab", "web"), img, len(layers), cache); err != nil {
		t.Fatal(err)
	}
	d, _ := layers[0].Digest()
	if !reg.HasBlobIn(ctx, Repo("ns", "lab", "web"), d) {
		t.Fatal("the base layer is in the device repository")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.Contains(p, "/v2/base/") || strings.HasPrefix(p, "DELETE ") {
			t.Errorf("a node touches no shared repository and deletes nothing: %s", p)
		}
	}
	for _, a := range auth {
		if a != "" {
			t.Errorf("the client sends no credentials: %q", a)
		}
	}
}

var inner = registry.New(registry.Logger(log.New(io.Discard, "", 0)))

// A refusal by a rule of the gate becomes a *Refusal with its code and its Retry-After; any other error stays as it is.
func TestPushReportsTheRefusalsOfTheGate(t *testing.T) {
	var refuse func(w http.ResponseWriter)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse != nil && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			refuse(w)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	reg := &Registry{Host: strings.TrimPrefix(srv.URL, "http://")}
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		status int
		code   string
		retry  string
		want   int // seconds
	}{
		"interval": {http.StatusTooManyRequests, CodePushInterval, "17", 17},
		"budget":   {http.StatusTooManyRequests, CodePushBudget, "", 0},
		"full":     {http.StatusInsufficientStorage, CodeRegistryFull, "", 0},
		"quota":    {http.StatusForbidden, CodeTenantQuota, "", 0},
	}
	for name, c := range cases {
		refuse = func(w http.ResponseWriter) {
			if c.retry != "" {
				w.Header().Set("Retry-After", c.retry)
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, `{"errors":[{"code":"`+c.code+`","message":"no"}]}`)
		}
		_, _, err := reg.Push(context.Background(), Repo("ns", "lab", "web"), img, 0, "")
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Status != c.status || ref.Code != c.code || int(ref.RetryAfter.Seconds()) != c.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	refuse = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"errors":[{"code":"UNKNOWN","message":"boom"}]}`)
	}
	_, _, err = reg.Push(context.Background(), Repo("ns", "lab", "web"), img, 0, "")
	var ref *Refusal
	if err == nil || errors.As(err, &ref) {
		t.Errorf("an ordinary failure is not a refusal: %v", err)
	}
}
