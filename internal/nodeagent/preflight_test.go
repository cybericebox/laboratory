package nodeagent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func registry(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func socket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pf") // a short path: a unix socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return path
}

func goodInput(t *testing.T) PreflightInput {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "io.containerd.snapshotter.v1.overlayfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cg, "cgroup.controllers"), []byte("cpu memory"), 0o644); err != nil {
		t.Fatal(err)
	}
	return PreflightInput{Persistence: true, Observation: true, ContainerdRoot: root, CRISock: socket(t), CgroupRoot: cg, RegistryAddr: registry(t, 401)}
}

func TestPreflightPassesOnAGoodNode(t *testing.T) {
	r := Preflight(context.Background(), goodInput(t))
	if r.StatePersistence != PreflightReady || r.RuntimeObservation != PreflightReady || len(r.Failures()) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestPreflightSaysWhatIsWrong(t *testing.T) {
	for name, tc := range map[string]struct {
		mod     func(*PreflightInput)
		persist string // the code, "" = ready
		observe string
	}{
		"containerd root missing (the k0s default on kind)": {mod: func(i *PreflightInput) { i.ContainerdRoot = "/var/lib/k0s/containerd-nope" }, persist: CodeContainerdRootMissing},
		"containerd root without a snapshotter":             {mod: func(i *PreflightInput) { i.ContainerdRoot = t.TempDir() }, persist: CodeSnapshotterMissing},
		"no root configured":                                {mod: func(i *PreflightInput) { i.ContainerdRoot = "" }, persist: CodeContainerdRootMissing},
		"CRI socket missing":                                {mod: func(i *PreflightInput) { i.CRISock = "/nonexistent.sock" }, persist: CodeRuntimeSocket, observe: CodeRuntimeSocket},
		"registry down":                                     {mod: func(i *PreflightInput) { i.RegistryAddr = "127.0.0.1:1" }, persist: CodeSnapshotRegistry},
		"registry broken":                                   {mod: func(i *PreflightInput) { i.RegistryAddr = registry(t, 503) }, persist: CodeSnapshotRegistry},
		"no cgroup v2 tree":                                 {mod: func(i *PreflightInput) { i.CgroupRoot = t.TempDir() }, observe: CodeCgroupV2Missing},
	} {
		t.Run(name, func(t *testing.T) {
			in := goodInput(t)
			tc.mod(&in)
			r := Preflight(context.Background(), in)
			check := func(what, got, want string) {
				code, bad := CodeOf(got)
				switch {
				case want == "" && got != PreflightReady:
					t.Errorf("%s: want ready, got %q", what, got)
				case want != "" && (!bad || code != want):
					t.Errorf("%s: want the code %s, got %q", what, want, got)
				}
				if why, _ := IsUnavailable(got); want != "" && why == "" {
					t.Errorf("%s: the detail for the operator is missing in %q", what, got)
				}
			}
			check("persistence", r.StatePersistence, tc.persist)
			check("observation", r.RuntimeObservation, tc.observe)
		})
	}
}

// What is not asked for is "disabled", not a failure; the annotations carry the results.
func TestPreflightOnlyChecksWhatIsAskedFor(t *testing.T) {
	r := Preflight(context.Background(), PreflightInput{ContainerdRoot: "/nope"})
	if r.StatePersistence != PreflightDisabled || r.RuntimeObservation != PreflightDisabled || len(r.Failures()) != 0 {
		t.Fatalf("%+v", r)
	}
	a := Unavailable(CodeRuntimeSocket, "x")
	if why, bad := IsUnavailable(a); !bad || why != "x" {
		t.Fatal(a)
	}
	if code, _ := CodeOf(a); code != CodeRuntimeSocket {
		t.Fatal(code)
	}
	// a value of an older node-agent, with no code, is unconfirmed
	if code, bad := CodeOf("unavailable: some old text"); !bad || code != CodeNodeUnconfirmed {
		t.Fatalf("%q %v", code, bad)
	}
}
