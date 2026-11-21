package grpc

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// The snapshots are not anonymous: the agent reads them with its own projected token, and cannot without it. It has no registry account.
func TestAgentReadsTheRegistryWithItsOwnToken(t *testing.T) {
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(imagecache.HeaderNodeToken) != "agent-token" || r.Header.Get("Authorization") != "" {
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
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("agent-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	h.SetRegistryTransport(nil, imagecache.NewTokenFile(tokenFile))
	if err := remote.Write(ref, img, h.registryOptions(context.Background())...); err != nil {
		t.Fatalf("with the token: %v", err)
	}
	if _, err := remote.Image(ref, h.registryOptions(context.Background())...); err != nil {
		t.Fatalf("read with the token: %v", err)
	}
	anonymous := &Handler{}
	if _, err := remote.Image(ref, anonymous.registryOptions(context.Background())...); err == nil {
		t.Fatal("a read of a snapshot without the token must be refused")
	}
}
