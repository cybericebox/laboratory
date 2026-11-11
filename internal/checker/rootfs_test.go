//go:build unix

package checker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const hostSecret = "HOST-SECRET-MUST-NEVER-BE-READ"

// tree makes a container root and, next to it, a directory of the "node" that holds a secret.
type tree struct {
	t       *testing.T
	root    string
	outside string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	base := t.TempDir()
	tr := &tree{t: t, root: filepath.Join(base, "root"), outside: filepath.Join(base, "outside")}
	for _, d := range []string{tr.root, tr.outside, filepath.Join(tr.root, "etc"), filepath.Join(tr.root, "app")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tr.file("etc/passwd", "root:x:0:0:root:/root:/bin/sh\nweb:x:1000:1001::/app:/bin/sh\n", 0o644)
	tr.file("etc/group", "root:x:0:\nweb:x:1001:\n", 0o644)
	tr.file("app/flag.txt", "ICE{inside}", 0o640)
	if err := os.WriteFile(filepath.Join(tr.outside, "secret"), []byte(hostSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	return tr
}

func (tr *tree) file(rel, content string, mode os.FileMode) {
	tr.t.Helper()
	p := filepath.Join(tr.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) link(rel, target string) {
	tr.t.Helper()
	p := filepath.Join(tr.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) open() *RootFS {
	tr.t.Helper()
	r, err := OpenRootFS(tr.root)
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.t.Cleanup(r.Close)
	return r
}

func TestObserveReadsFilesInsideTheRoot(t *testing.T) {
	tr := newTree(t)
	r := tr.open()
	f, err := r.Observe("/app/flag.txt", true, IDMap{})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Exists || !f.Regular || string(f.Content) != "ICE{inside}" || f.Mode != 0o640 {
		t.Fatalf("got %+v", f)
	}
	r.Names(&f)
	if f.UID != uint32(os.Getuid()) {
		t.Fatalf("uid %d, want %d", f.UID, os.Getuid())
	}
}

func TestObserveMissingAndDirectory(t *testing.T) {
	tr := newTree(t)
	r := tr.open()
	for _, p := range []string{"/nope", "/app/nope/deeper", "/app/flag.txt/not-a-dir"} {
		f, err := r.Observe(p, true, IDMap{})
		if err != nil || f.Exists {
			t.Errorf("%s: %+v, %v; want a missing file", p, f, err)
		}
	}
	f, err := r.Observe("/app", true, IDMap{})
	if err != nil || !f.Exists || f.Regular || f.Content != nil {
		t.Errorf("directory: %+v, %v", f, err)
	}
	f, err = r.Observe("/", false, IDMap{})
	if err != nil || !f.Exists || f.Regular {
		t.Errorf("root: %+v, %v", f, err)
	}
}

// The security requirement: nothing the participant plants may reach a file outside the container's root.
func TestSymlinksNeverLeaveTheRoot(t *testing.T) {
	tr := newTree(t)
	secretAbs := filepath.Join(tr.outside, "secret")
	rel, err := filepath.Rel(tr.root, secretAbs)
	if err != nil {
		t.Fatal(err)
	}
	tr.link("evil-abs", secretAbs)                                                         // an absolute link to a node file
	tr.link("evil-rel", rel)                                                               // a relative link that climbs out
	tr.link("app/evil-deep", strings.Repeat("../", 40)+strings.TrimPrefix(secretAbs, "/")) // far more ".." than levels
	tr.link("evil-dir", tr.outside)                                                        // a link to the node directory
	tr.link("evil-dotdot", "app/../../outside/secret")                                     // ".." in the middle
	for _, p := range []string{"/evil-abs", "/evil-rel", "/app/evil-deep", "/evil-dir/secret", "/evil-dotdot", "/app/../../outside/secret", "/../../" + strings.TrimPrefix(secretAbs, "/")} {
		r := tr.open()
		f, err := r.Observe(p, true, IDMap{})
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if f.Exists || bytes.Contains(f.Content, []byte(hostSecret)) {
			t.Errorf("%s reached outside the root: %+v", p, f)
		}
	}
}

func TestAbsoluteSymlinkMeansTheContainersRoot(t *testing.T) {
	tr := newTree(t)
	tr.link("abs-passwd", "/etc/passwd")
	tr.link("app/rel-flag", "flag.txt")
	tr.link("app/up", "../etc")
	tr.link("rootlink", "/")
	r := tr.open()
	for p, want := range map[string]string{
		"/abs-passwd":               "root:x:0:0",
		"/app/rel-flag":             "ICE{inside}",
		"/app/up/passwd":            "root:x:0:0",
		"/rootlink/etc/passwd":      "root:x:0:0",
		"/rootlink/../app/flag.txt": "ICE{inside}",
	} {
		f, err := r.Observe(p, true, IDMap{})
		if err != nil || !f.Exists || !strings.HasPrefix(string(f.Content), want) {
			t.Errorf("%s: %+v, %v; want content starting %q", p, f, err, want)
		}
	}
}

func TestDanglingAndLoopingLinks(t *testing.T) {
	tr := newTree(t)
	tr.link("dangling", "/does/not/exist")
	tr.link("loop-a", "/loop-b")
	tr.link("loop-b", "/loop-a")
	r := tr.open()
	if f, err := r.Observe("/dangling", true, IDMap{}); err != nil || f.Exists {
		t.Errorf("dangling: %+v, %v", f, err)
	}
	if _, err := r.Observe("/loop-a", true, IDMap{}); !errors.Is(err, ErrTooManyLinks) {
		t.Errorf("loop: %v, want ErrTooManyLinks", err)
	}
}

// A path through /proc (or /sys, /dev) of the container is refused, however it is reached: the node cannot see the container's own
// mounts, and the node's own /proc would answer for the checker instead.
func TestPseudoFilesystemsAreRefused(t *testing.T) {
	tr := newTree(t)
	tr.link("to-proc", "/proc")
	tr.link("app/to-sys", "../sys")
	r := tr.open()
	for _, p := range []string{"/proc/1/root/etc/passwd", "/proc/self/environ", "/sys/class/net", "/dev/null/x", "/to-proc/self/root", "/app/to-sys/kernel", "/etc/../proc/cpuinfo"} {
		if _, err := r.Observe(p, true, IDMap{}); !errors.Is(err, ErrPseudoPath) {
			t.Errorf("%s: %v, want ErrPseudoPath", p, err)
		}
	}
	// The mount point itself is a plain directory in the root file system.
	tr2 := newTree(t)
	if err := os.Mkdir(filepath.Join(tr2.root, "proc"), 0o555); err != nil {
		t.Fatal(err)
	}
	f, err := tr2.open().Observe("/proc", false, IDMap{})
	if err != nil || !f.Exists {
		t.Errorf("/proc itself: %+v, %v", f, err)
	}
}

func TestFifoIsNeverWaitedOn(t *testing.T) {
	tr := newTree(t)
	if err := unix.Mkfifo(filepath.Join(tr.root, "app", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := tr.open().Observe("/app/pipe", true, IDMap{})
	if err != nil || !f.Exists || f.Regular || f.Content != nil {
		t.Fatalf("fifo: %+v, %v", f, err)
	}
}

func TestContentIsBounded(t *testing.T) {
	tr := newTree(t)
	tr.file("app/big", strings.Repeat("a", 5000), 0o644)
	r := tr.open()
	r.MaxContent = 1000
	f, err := r.Observe("/app/big", true, IDMap{})
	if err != nil || len(f.Content) != 1000 || !f.Truncated {
		t.Fatalf("got %d bytes, truncated %v, err %v", len(f.Content), f.Truncated, err)
	}
	f, err = r.Observe("/app/big", false, IDMap{})
	if err != nil || f.Content != nil || f.Truncated {
		t.Fatalf("no content asked: %+v, %v", f, err)
	}
}

func TestIDMapTranslatesHostNumbers(t *testing.T) {
	m := IDMap{UIDs: []IDRange{{ContainerID: 0, HostID: 65536, Size: 65536}}, GIDs: []IDRange{{ContainerID: 0, HostID: 65536, Size: 1000}}}
	for _, c := range []struct {
		host   uint32
		want   uint32
		wantOK bool
	}{{65536, 0, true}, {66536, 1000, true}, {131071, 65535, true}, {131072, 0, false}, {5, 0, false}} {
		got, ok := m.UID(c.host)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("UID(%d) = %d, %v", c.host, got, ok)
		}
	}
	if _, ok := m.GID(66536); ok {
		t.Error("gid 66536 is outside the gid range")
	}
	if got, ok := (IDMap{}).UID(1234); !ok || got != 1234 {
		t.Errorf("empty map must be the identity: %d, %v", got, ok)
	}
}

// A link that expands into an enormous path costs a bounded number of steps.
func TestPathWorkIsBounded(t *testing.T) {
	tr := newTree(t)
	if err := os.MkdirAll(filepath.Join(tr.root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 39 links, each to a path of 300 components: far over the step limit.
	long := strings.Repeat("d/../", 150)
	for i := 0; i < 39; i++ {
		next := fmt.Sprintf("/hop%d", i+1)
		if i == 38 {
			next = "/app/flag.txt"
		}
		tr.link(fmt.Sprintf("hop%d", i), long+strings.TrimPrefix(next, "/"))
	}
	_, err := tr.open().Observe("/hop0", true, IDMap{})
	if !errors.Is(err, ErrTooManyLinks) {
		t.Fatalf("got %v", err)
	}
}
