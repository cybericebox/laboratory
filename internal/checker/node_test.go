//go:build unix

package checker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

type fakeCRI struct {
	info     ContainerInfo
	findErr  error
	answer   func(cmd []string) ([]byte, int32, error)
	asked    [][]string
	timeouts []time.Duration
	podUID   string
	name     string
}

func (f *fakeCRI) FindContainer(_ context.Context, podUID, container string) (ContainerInfo, error) {
	f.podUID, f.name = podUID, container
	return f.info, f.findErr
}

func (f *fakeCRI) ExecSync(_ context.Context, id string, cmd []string, timeout time.Duration) ([]byte, int32, error) {
	f.asked = append(f.asked, append([]string{id}, cmd...))
	f.timeouts = append(f.timeouts, timeout)
	return f.answer(cmd)
}
func (f *fakeCRI) Close() error { return nil }

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNodeBackendReadsContainerFilesFromTheNode(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "io.containerd.runtime.v2.task", "k8s.io", "cid1", "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"etc/passwd": "root:x:0:0::/root:/bin/sh\n", "etc/group": "root:x:0:\n", "etc/ssh_config": "PermitRootLogin no\n"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte(c), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, p), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// The files belong to our own host user; inside the container that user is root (a user namespace).
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	cri := &fakeCRI{info: ContainerInfo{ID: "cid1", IDs: IDMap{UIDs: []IDRange{{0, uid, 1}}, GIDs: []IDRange{{0, gid, 1}}}}}
	b := &NodeBackend{CRI: cri, StateDir: state, Namespace: "k8s.io"}
	obs, release, err := b.Open(context.Background(), PodInfo{UID: "pod-uid", Device: "web"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if cri.podUID != "pod-uid" || cri.name != "web" {
		t.Fatalf("looked for %q %q", cri.podUID, cri.name)
	}
	c := &labchecks.Check{ID: "c", TimeLimitSeconds: 5, Parts: []labchecks.Part{{File: &labchecks.FilePart{
		Path: "/etc/ssh_config", ContentContains: sp("PermitRootLogin no"), Mode: mp(0o640), Owner: sp("root"), Group: sp("0")}}}}
	if r := RunCheck(context.Background(), c, obs); r.Outcome != Passed {
		t.Fatalf("got %+v", r)
	}
	c.Parts[0].File.Owner = sp("1000")
	if r := RunCheck(context.Background(), c, obs); r.Outcome != NotPassed || !strings.Contains(r.Detail, "owner") {
		t.Fatalf("got %+v", r)
	}
	// Nothing was executed in the container to read a file.
	if len(cri.asked) != 0 {
		t.Fatalf("a file check ran %v in the container", cri.asked)
	}
}

func TestNodeBackendReportsAMissingRootAndAStoppedContainer(t *testing.T) {
	cri := &fakeCRI{info: ContainerInfo{ID: "gone"}}
	b := &NodeBackend{CRI: cri, StateDir: t.TempDir(), Namespace: "k8s.io"}
	if _, _, err := b.Open(context.Background(), PodInfo{}); err == nil || reasonOf(context.Background(), err) != "the node cannot read the files of the container" {
		t.Fatalf("got %v", err)
	}
	cri.findErr = &ObserveError{Reason: "the container is not running"}
	if _, _, err := b.Open(context.Background(), PodInfo{}); reasonOf(context.Background(), err) != "the container is not running" {
		t.Fatalf("got %v", err)
	}
}

func TestExecGoesThroughTheRuntimeWithTheInDeviceTool(t *testing.T) {
	cri := &fakeCRI{info: ContainerInfo{ID: "cid1"}, answer: func([]string) ([]byte, int32, error) {
		return jsonOf(t, probe.ExecResponse{Started: true, Exit: 0, Output: []byte("active")}), 0, nil
	}}
	b := &NodeBackend{CRI: cri, StateDir: t.TempDir(), Namespace: "k8s.io"}
	dir := filepath.Join(b.StateDir, "io.containerd.runtime.v2.task", "k8s.io", "cid1", "rootfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	obs, release, err := b.Open(context.Background(), PodInfo{UID: "u", Device: "web"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := obs.Exec(ctx, ExecSpec{Command: []string{"systemctl", "is-active", "x"}, User: "www-data", Limit: 10 * time.Second})
	if err != nil || !res.Started || string(res.Output) != "active" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(cri.asked) != 1 {
		t.Fatalf("asked %v", cri.asked)
	}
	got := cri.asked[0]
	if got[0] != "cid1" || got[1] != names.ProbeBinary || got[2] != probe.CommandCheckExec {
		t.Fatalf("command line %v", got)
	}
	var req probe.ExecRequest
	if err := json.Unmarshal([]byte(got[3]), &req); err != nil || req.User != "www-data" || strings.Join(req.Argv, " ") != "systemctl is-active x" || req.TimeoutMs != 10000 {
		t.Fatalf("request %+v, %v", req, err)
	}
	if cri.timeouts[0] <= 10*time.Second {
		t.Fatalf("the exec must outlast the command's own limit: %v", cri.timeouts[0])
	}
}

func TestMicroVMDeviceFilesGoThroughTheInDeviceTool(t *testing.T) {
	cri := &fakeCRI{info: ContainerInfo{ID: "kata1"}, answer: func(cmd []string) ([]byte, int32, error) {
		if cmd[1] != probe.CommandCheckFile {
			t.Errorf("unexpected %v", cmd)
		}
		return jsonOf(t, probe.FileResponse{Exists: true, Regular: true, Mode: 0o600, User: "root", Group: "root", Content: []byte("on")}), 0, nil
	}}
	b := &NodeBackend{CRI: cri, StateDir: "/does/not/exist", Namespace: "k8s.io"}
	obs, release, err := b.Open(context.Background(), PodInfo{UID: "u", Device: "vm", MicroVM: true})
	if err != nil {
		t.Fatalf("a micro-VM device needs no root directory on the node: %v", err)
	}
	defer release()
	c := &labchecks.Check{ID: "c", TimeLimitSeconds: 5, Parts: []labchecks.Part{{File: &labchecks.FilePart{Path: "/etc/x", ContentContains: sp("on"), Mode: mp(0o600), Owner: sp("root")}}}}
	if r := RunCheck(context.Background(), c, obs); r.Outcome != Passed {
		t.Fatalf("got %+v", r)
	}
	if len(cri.asked) != 1 {
		t.Fatalf("asked %v", cri.asked)
	}
}

func TestMissingToolIsAnErrorNotAFailedCheck(t *testing.T) {
	for name, answer := range map[string]func([]string) ([]byte, int32, error){
		"exit 127":    func([]string) ([]byte, int32, error) { return nil, 127, nil },
		"not json":    func([]string) ([]byte, int32, error) { return []byte("hello"), 0, nil },
		"exec failed": func([]string) ([]byte, int32, error) { return nil, 0, os.ErrDeadlineExceeded },
	} {
		cri := &fakeCRI{info: ContainerInfo{ID: "k"}, answer: answer}
		obs := &execObserver{cri: cri, containerID: "k"}
		c := &labchecks.Check{ID: "c", TimeLimitSeconds: 5, Parts: []labchecks.Part{{Custom: &labchecks.CustomPart{Command: []string{"true"}}}}}
		if r := RunCheck(context.Background(), c, obs); r.Outcome != Failed {
			t.Errorf("%s: %+v", name, r)
		}
	}
}
