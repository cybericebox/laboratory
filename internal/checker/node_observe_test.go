//go:build unix

package checker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
