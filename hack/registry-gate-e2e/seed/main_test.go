package main

import (
	"archive/tar"
	"io"
	"testing"
)

// The image runs a binary: one layer holds /sleeper (executable), the entrypoint names it, and two images of different markers differ.
func TestTheImageRunsTheSleeperAndDiffersByMarker(t *testing.T) {
	bin := []byte("\x7fELF fake binary")
	a, err := image(bin, "arm64", "team/app")
	if err != nil {
		t.Fatal(err)
	}
	b, err := image(bin, "arm64", "team/secret")
	if err != nil {
		t.Fatal(err)
	}
	da, _ := a.Digest()
	db, _ := b.Digest()
	if da == db {
		t.Fatal("the two images must differ")
	}
	cf, err := a.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Architecture != "arm64" || cf.OS != "linux" || len(cf.Config.Entrypoint) != 1 || cf.Config.Entrypoint[0] != "/sleeper" {
		t.Fatalf("%+v", cf)
	}
	layers, err := a.Layers()
	if err != nil || len(layers) != 1 {
		t.Fatalf("%d layers, %v", len(layers), err)
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatal("no /sleeper in the layer")
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "sleeper" {
			if h.Mode&0o111 == 0 {
				t.Fatalf("not executable: %o", h.Mode)
			}
			return
		}
	}
}
