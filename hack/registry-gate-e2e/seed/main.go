// Command seed pushes a small random image into each of two upstream registries of the registry gate E2E (one open,
// one with Basic authentication) and prints their digests as JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func push(addr, repo string, auth authn.Authenticator) (string, error) {
	img, err := random.Image(4096, 2)
	if err != nil {
		return "", err
	}
	// A command in the image config: without one containerd cannot make the container spec ("no command specified") and the pod
	// stays in CreateContainerError. With one the container is created and then fails to start (the layers are random bytes),
	// which is fine: the E2E asks whether the image reached the node.
	if img, err = mutate.Config(img, v1.Config{Entrypoint: []string{"/e2e-noop"}}); err != nil {
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
	flag.Parse()
	out := map[string]string{}
	var err error
	if out["pub"], err = push(*pub, "team/app", authn.Anonymous); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if out["priv"], err = push(*priv, "team/secret", &authn.Basic{Username: *user, Password: *pass}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
