package registrygate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRegistries(t *testing.T) {
	list, err := ParseRegistries([]byte(`[{"name":"docker.io","url":"https://registry-1.docker.io"},{"name":"reg.example.com","url":"http://reg.svc:5000"}]`))
	if err != nil || len(list) != 2 || list[0].Name != "docker.io" || list[1].URL.Host != "reg.svc:5000" {
		t.Fatalf("%v %+v", err, list)
	}
	for _, bad := range []string{`not json`, `[{"name":"","url":"https://a"}]`, `[{"name":"a","url":"ftp://a"}]`, `[{"name":"a","url":"a"}]`, `[{"name":"a"}]`} {
		if _, err := ParseRegistries([]byte(bad)); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

// The list is replaced when the file changes, and a broken file keeps the list in force.
func TestRegistrySetWatchesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registries.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"name":"docker.io","url":"https://registry-1.docker.io"}]`)
	var set RegistrySet
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 10)
	if err := set.Watch(ctx, path, 10*time.Millisecond, func(err error) { errs <- err }); err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Get("docker.io"); !ok {
		t.Fatal("loaded at once")
	}
	write(`[{"name":"ghcr.io","url":"https://ghcr.io"}]`)
	waitFor(t, func() bool { _, ok := set.Get("ghcr.io"); return ok })
	if _, ok := set.Get("docker.io"); ok {
		t.Fatal("the new list replaces the old one")
	}
	write(`broken`)
	select {
	case <-errs:
	case <-time.After(2 * time.Second):
		t.Fatal("a broken file is reported")
	}
	if _, ok := set.Get("ghcr.io"); !ok {
		t.Fatal("a broken file keeps the list in force")
	}
	if err := (&RegistrySet{}).Watch(ctx, filepath.Join(t.TempDir(), "none"), time.Second, nil); err == nil {
		t.Fatal("a file that is not there at the start is an error")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
