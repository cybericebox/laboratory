package grpc

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// The snapshots are not anonymous: the agent reads them with the reader account, and cannot without it.
func TestAgentReadsTheRegistryWithTheReaderAccount(t *testing.T) {
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "reader" || pass != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host+"/lab/ns/lab/dev:latest", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	h.SetRegistryAuth("reader", "pw")
	if err := remote.Write(ref, img, h.registryOptions(context.Background())...); err != nil {
		t.Fatalf("with the account: %v", err)
	}
	if _, err := remote.Image(ref, h.registryOptions(context.Background())...); err != nil {
		t.Fatalf("read with the account: %v", err)
	}
	anonymous := &Handler{}
	if _, err := remote.Image(ref, anonymous.registryOptions(context.Background())...); err == nil {
		t.Fatal("an anonymous read of a snapshot must be refused")
	}
	h.SetRegistryAuth("", "")
	if h.registryAuth != nil {
		t.Fatal("an empty user means anonymous")
	}
}
