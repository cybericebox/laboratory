// Command seed pushes a small runnable image into each of two upstream registries of the registry gate E2E (one open,
// one with Basic authentication) and prints their digests as JSON.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// image is one layer with a static binary that keeps the container running, and a marker file,
// so that the images of the two registries differ (different layer, different digest) and
// every run seeds its own.
func image(sleeper []byte, arch, marker string) (v1.Image, error) {
	rnd := make([]byte, 2048)
	if _, err := rand.Read(rnd); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{{"sleeper", 0o755, sleeper}, {"marker", 0o644, append([]byte(marker+"\n"), rnd...)}} {
		hdr := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil }
	layer, err := tarball.LayerFromOpener(open)
	if err != nil {
		return nil, err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", arch
	cf.Config.Entrypoint = []string{"/sleeper"}
	return mutate.ConfigFile(img, cf)
}

func push(addr, repo string, auth authn.Authenticator, sleeper []byte, arch string) (string, error) {
	img, err := image(sleeper, arch, repo)
	if err != nil {
		return "", err
	}
	ref, err := name.NewTag(addr+"/"+repo+":seed", name.Insecure)
	if err != nil {
		return "", err
	}
	if err := remote.Write(ref, img, remote.WithAuth(auth)); err != nil {
		return "", err
	}
	d, err := img.Digest()
	if err != nil {
		return "", err
	}
	return d.String(), nil
}

func main() {
	pub := flag.String("pub", "", "address of the open registry")
	priv := flag.String("priv", "", "address of the registry with Basic authentication")
	user := flag.String("user", "alice", "user of the second one")
	pass := flag.String("pass", "", "its password")
	sleeperPath := flag.String("sleeper", "", "the static binary that the image runs (built for linux/<arch>)")
	arch := flag.String("arch", "amd64", "the architecture of the node")
	flag.Parse()
	sleeper, err := os.ReadFile(*sleeperPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out := map[string]string{}
	if out["pub"], err = push(*pub, "team/app", authn.Anonymous, sleeper, *arch); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	basic := &authn.Basic{Username: *user, Password: *pass}
	if out["priv"], err = push(*priv, "team/secret", basic, sleeper, *arch); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
