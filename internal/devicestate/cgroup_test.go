package devicestate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCgroupDirSystemdAndCgroupfs(t *testing.T) {
	root := t.TempDir()
	want := mkdir(t, root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod1234_abcd.slice", "cri-containerd-deadbeef.scope")
	got := CgroupDir(root, "kubepods-burstable-pod1234_abcd.slice:cri-containerd:deadbeef")
	if got != want {
		t.Fatalf("systemd path: got %q want %q", got, want)
	}
	wantFS := mkdir(t, root, "kubepods", "burstable", "pod1234", "deadbeef")
	if got := CgroupDir(root, "/kubepods/burstable/pod1234/deadbeef"); got != wantFS {
		t.Fatalf("cgroupfs path: got %q want %q", got, wantFS)
	}
	if CgroupDir(root, "kubepods-besteffort-pod9.slice:cri-containerd:none") != "" || CgroupDir(root, "") != "" {
		t.Fatal("a missing directory must resolve to empty")
	}
}

func TestExpandSlice(t *testing.T) {
	if got := expandSlice("kubepods-burstable-podX.slice"); got != "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podX.slice" {
		t.Fatal(got)
	}
	if expandSlice("-.slice") != "" || expandSlice("") != "" {
		t.Fatal("root slice expands to nothing")
	}
}

func TestFindCgroup(t *testing.T) {
	root := t.TempDir()
	want := mkdir(t, root, "a", "b", "cri-containerd-0123abcd.scope")
	mkdir(t, root, "a", "other")
	if got := FindCgroup(root, "0123abcd"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if FindCgroup(root, "ffff") != "" {
		t.Fatal("unknown container must not be found")
	}
}

// fakeCgroup emulates the kernel: writing 1 to cgroup.freeze makes cgroup.events report frozen.
func fakeCgroup(t *testing.T, frozenOnWrite bool) string {
	t.Helper()
	dir := mkdir(t, t.TempDir(), "cri-containerd-x.scope")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "cgroup.freeze"), []byte("0"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte("populated 1\nfrozen 0\n"), 0o644))
	return dir
}

func TestFreezeWaitsForFrozenAndThaws(t *testing.T) {
	dir := fakeCgroup(t, true)
	// The "kernel": when freeze is requested, flip the events file.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, _ := os.ReadFile(filepath.Join(dir, "cgroup.freeze"))
			ev := "populated 1\nfrozen 0\n"
			if strings.TrimSpace(string(b)) == "1" {
				ev = "populated 1\nfrozen 1\n"
			}
			_ = os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte(ev), 0o644)
		}
	}()
	defer close(stop)

	thaw, err := Freeze(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cgroup.freeze")); strings.TrimSpace(string(b)) != "1" {
		t.Fatal("cgroup.freeze must be 1 while frozen")
	}
	thaw()
	if b, _ := os.ReadFile(filepath.Join(dir, "cgroup.freeze")); strings.TrimSpace(string(b)) != "0" {
		t.Fatal("thaw must write 0")
	}
}

func TestFreezeWithoutFreezerIsReported(t *testing.T) {
	if _, err := Freeze(context.Background(), ""); err != ErrNoFreezer {
		t.Fatalf("got %v", err)
	}
	if _, err := Freeze(context.Background(), t.TempDir()); err != ErrNoFreezer {
		t.Fatalf("a cgroup without cgroup.freeze (v1) must report ErrNoFreezer, got %v", err)
	}
}

func TestThawOrphans(t *testing.T) {
	root := t.TempDir()
	stuck := mkdir(t, root, "kubepods.slice", "pod.slice", "cri-containerd-aaa.scope")
	fine := mkdir(t, root, "kubepods.slice", "pod.slice", "cri-containerd-bbb.scope")
	other := mkdir(t, root, "system.slice", "docker.service")
	for dir, ev := range map[string]string{stuck: "frozen 1\n", fine: "frozen 0\n", other: "frozen 1\n"} {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte(ev), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "cgroup.freeze"), []byte("1"), 0o644)
	}
	got := ThawOrphans(root)
	if len(got) != 1 || got[0] != stuck {
		t.Fatalf("thawed %v, want only %s", got, stuck)
	}
	if b, _ := os.ReadFile(filepath.Join(stuck, "cgroup.freeze")); strings.TrimSpace(string(b)) != "0" {
		t.Fatal("orphan not thawed")
	}
	if b, _ := os.ReadFile(filepath.Join(other, "cgroup.freeze")); strings.TrimSpace(string(b)) != "1" {
		t.Fatal("a cgroup that is not a container scope must be left alone")
	}
}
