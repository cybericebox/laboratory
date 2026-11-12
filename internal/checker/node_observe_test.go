//go:build unix

package checker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

// Both kinds of device answer the built-in observations through the in-device program, started by the runtime's exec.
func TestBuiltinsGoThroughTheInDeviceProgram(t *testing.T) {
	for _, microVM := range []bool{false, true} {
		cri := &fakeCRI{info: ContainerInfo{ID: "cid"}, answer: func(cmd []string) ([]byte, int32, error) {
			return jsonOf(t, probe.ObserveResponse{KernelParam: &probe.KernelParamFacts{Exists: true, Value: "1"}}), 0, nil
		}}
		b := &NodeBackend{CRI: cri, StateDir: t.TempDir(), Namespace: "k8s.io"}
		if err := os.MkdirAll(filepath.Join(b.RootDir("cid")), 0o755); err != nil {
			t.Fatal(err)
		}
		obs, release, err := b.Open(context.Background(), PodInfo{UID: "u", Device: "d", MicroVM: microVM})
		if err != nil {
			t.Fatal(err)
		}
		c := check(labchecks.Part{KernelParam: &labchecks.KernelParamPart{Name: "net.ipv4.ip_forward", Equals: sp("1")}})
		if r := RunCheck(context.Background(), c, obs); r.Outcome != Passed {
			t.Fatalf("microVM=%v: %+v", microVM, r)
		}
		release()
		if len(cri.asked) != 1 || cri.asked[0][1] != names.ProbeBinary || cri.asked[0][2] != probe.CommandCheckObserve {
			t.Fatalf("asked %v", cri.asked)
		}
		var q probe.ObserveRequest
		if err := json.Unmarshal([]byte(cri.asked[0][3]), &q); err != nil || q.Kind != probe.KindKernelParam || q.Name != "net.ipv4.ip_forward" {
			t.Fatalf("question %+v %v", q, err)
		}
		if cri.asked[0][3] != `{"kind":"kernel_param","name":"net.ipv4.ip_forward"}` {
			t.Fatalf("the question carries more than where to look: %s", cri.asked[0][3])
		}
	}
}

// A file expectation of type or checksum is answered by the in-device program on every device; the node-side reader, which follows links,
// is not asked, so there is one behaviour.
func TestTypeAndChecksumGoThroughTheProbe(t *testing.T) {
	cri := &fakeCRI{info: ContainerInfo{ID: "cid"}, answer: func(cmd []string) ([]byte, int32, error) {
		return jsonOf(t, probe.FileResponse{Exists: true, Regular: true, Type: "symlink", Content: []byte("hello")}), 0, nil
	}}
	b := &NodeBackend{CRI: cri, StateDir: t.TempDir(), Namespace: "k8s.io"}
	root := b.RootDir("cid")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("on the node"), 0o644); err != nil {
		t.Fatal(err)
	}
	obs, release, err := b.Open(context.Background(), PodInfo{UID: "u", Device: "d"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	typed := check(labchecks.Part{File: &labchecks.FilePart{Path: "/f", Type: sp("symlink")}})
	if r := RunCheck(context.Background(), typed, obs); r.Outcome != Passed || len(cri.asked) != 1 || cri.asked[0][2] != probe.CommandCheckFile {
		t.Fatalf("type: %+v after %v", r, cri.asked)
	}
	sum := sha256Of([]byte("hello"))
	summed := check(labchecks.Part{File: &labchecks.FilePart{Path: "/f", Checksum: &sum}})
	if r := RunCheck(context.Background(), summed, obs); r.Outcome != Passed || len(cri.asked) != 2 {
		t.Fatalf("checksum: %+v after %v", r, cri.asked)
	}
	// Without them the node-side reader is used and nothing is executed in the device.
	plain := check(labchecks.Part{File: &labchecks.FilePart{Path: "/f", ContentContains: sp("on the node")}})
	if r := RunCheck(context.Background(), plain, obs); r.Outcome != Passed || len(cri.asked) != 2 {
		t.Fatalf("plain: %+v after %v", r, cri.asked)
	}
	// An observer that cannot ask the program cannot judge type or checksum.
	if r := RunCheck(context.Background(), typed, &fakeObserver{files: map[string]FileFacts{"/f": present("x")}}); r.Outcome != Failed {
		t.Fatalf("got %+v", r)
	}
}

// The exact bytes sent to the device, for every kind of observation: where to look, and not one thing the author expects. The comparison
// is the checker's alone.
func TestNothingThatIsExpectedIsSentToTheDevice(t *testing.T) {
	const m = "ZQXJ-EXPECTED-"
	str := func(tag string) *string { return sp(m + tag) }
	status := i32p(599)
	parts := map[string]labchecks.Part{
		"file": {File: &labchecks.FilePart{Path: "/etc/app.conf", Exists: bp(true), ContentContains: str("contains"), ContentMatches: str("matches"), Mode: mp(0o7777),
			Owner: str("owner"), Group: str("group"), Type: str("type-not-valid-but-sent-nowhere"), Checksum: str("checksum")}},
		"directory": {Directory: &labchecks.DirectoryPart{Path: "/srv/app", Exists: bp(true), Mode: mp(0o7777), Owner: str("owner"), Group: str("group"),
			HasEntries: []string{m + "has"}, LacksEntries: []string{m + "lacks"}}},
		"process":      {Process: &labchecks.ProcessPart{Name: str("name"), Matches: str("pattern"), User: str("user"), Running: bp(false)}},
		"port":         {Port: &labchecks.PortPart{Port: 8080, Protocol: "udp", Listening: bp(false), Address: str("address"), Process: str("process")}},
		"kernel_param": {KernelParam: &labchecks.KernelParamPart{Name: "net.ipv4.ip_forward", Equals: str("equals"), Matches: str("matches")}},
		"request": {Request: &labchecks.RequestPart{Protocol: "http", Host: "127.0.0.1", Port: 8080, Method: "GET", Path: "/health", Status: status,
			BodyContains: str("body-contains"), BodyMatches: str("body-matches")}},
		"permission": {Permission: &labchecks.PermissionPart{User: "www-data", Path: "/etc/shadow", Read: bp(false), Write: bp(true), Execute: bp(false)}},
		"custom":     {Custom: &labchecks.CustomPart{Command: []string{"/usr/bin/true"}, User: "nobody", OutputContains: str("out-contains"), OutputMatches: str("out-matches")}},
	}
	for kind, part := range parts {
		for _, microVM := range []bool{false, true} {
			cri := &fakeCRI{info: ContainerInfo{ID: "cid"}, answer: func(cmd []string) ([]byte, int32, error) { return []byte("{}"), 0, nil }}
			b := &NodeBackend{CRI: cri, StateDir: t.TempDir(), Namespace: "k8s.io"}
			if err := os.MkdirAll(b.RootDir("cid"), 0o755); err != nil {
				t.Fatal(err)
			}
			obs, release, err := b.Open(context.Background(), PodInfo{UID: "u", Device: "d", MicroVM: microVM})
			if err != nil {
				t.Fatal(err)
			}
			RunCheck(context.Background(), check(part), obs)
			release()
			if kind != "file" && len(cri.asked) != 1 {
				t.Fatalf("%s: %d commands run in the device", kind, len(cri.asked))
			}
			for _, cmd := range cri.asked {
				all := strings.Join(cmd, "\x00")
				for _, bad := range []string{m, "4095", "599", "7777"} {
					if strings.Contains(all, bad) {
						t.Errorf("%s (microVM=%v): the device was sent %q in %s", kind, microVM, bad, all)
					}
				}
				if kind == "directory" && !strings.Contains(all, `"key"`) {
					t.Errorf("directory: the question has no key: %s", all)
				}
			}
		}
	}
}
