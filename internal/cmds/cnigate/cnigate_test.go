package cnigate

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
)

func TestParsePodArgs(t *testing.T) {
	ns, name, uid := parsePodArgs("IgnoreUnknown=1;K8S_POD_NAMESPACE=lab-ns;K8S_POD_NAME=dev-1;K8S_POD_UID=abc-123")
	if ns != "lab-ns" || name != "dev-1" || uid != "abc-123" {
		t.Fatalf("got (%q, %q, %q)", ns, name, uid)
	}
}

func TestParsePodArgs_Missing(t *testing.T) {
	ns, name, uid := parsePodArgs("IgnoreUnknown=1")
	if ns != "" || name != "" || uid != "" {
		t.Fatalf("expected empty, got (%q, %q, %q)", ns, name, uid)
	}
}

func TestParsePodArgs_MalformedEntries(t *testing.T) {
	// Entries without "=" must be skipped, valid ones still parsed.
	ns, name, _ := parsePodArgs("garbage;K8S_POD_NAMESPACE=ns;;K8S_POD_NAME=pod")
	if ns != "ns" || name != "pod" {
		t.Fatalf("got (%q, %q)", ns, name)
	}
}

func ipConfig(ifIdx int, cidr string) *cniv1.IPConfig {
	_, ipNet, _ := net.ParseCIDR(cidr)
	return &cniv1.IPConfig{Interface: cniv1.Int(ifIdx), Address: *ipNet}
}

func TestEnsureEth0_AlreadyOwnsIP(t *testing.T) {
	base := &cniv1.Result{
		Interfaces: []*cniv1.Interface{{Name: "eth0", Sandbox: "/ns/1"}},
		IPs:        []*cniv1.IPConfig{ipConfig(0, "10.244.0.5/32")},
	}
	got := ensureEth0(base, "/ns/1")
	if len(got.Interfaces) != 1 || len(got.IPs) != 1 {
		t.Fatalf("result must be unchanged: %+v", got)
	}
	if got.IPs[0].Address.IP.String() != "10.244.0.5" {
		t.Fatalf("real IP must be preserved, got %s", got.IPs[0].Address.IP)
	}
}

func TestEnsureEth0_PresentWithoutIP(t *testing.T) {
	base := &cniv1.Result{
		Interfaces: []*cniv1.Interface{
			{Name: "eth0", Sandbox: "/ns/1"},
			{Name: "accessport", Sandbox: "/ns/1"},
		},
		IPs: []*cniv1.IPConfig{ipConfig(1, "10.244.0.5/32")},
	}
	got := ensureEth0(base, "/ns/1")
	if len(got.IPs) != 2 {
		t.Fatalf("expected stub IP appended, got %d IPs", len(got.IPs))
	}
	stub := got.IPs[1]
	if stub.Interface == nil || *stub.Interface != 0 {
		t.Fatalf("stub IP must point at eth0 (index 0), got %v", stub.Interface)
	}
	if stub.Address.IP.String() != "127.0.0.1" {
		t.Fatalf("stub IP must be 127.0.0.1, got %s", stub.Address.IP)
	}
	// Real IP must still point at accessport (index 1).
	if *got.IPs[0].Interface != 1 {
		t.Fatalf("real IP index must be untouched, got %d", *got.IPs[0].Interface)
	}
}

func TestEnsureEth0_AbsentWithRealIP(t *testing.T) {
	// Access-port pod: delegate wired the real IP onto "accessport". The result
	// must report the REAL IP on eth0 so Kubernetes uses it as pod.status.podIP —
	// a stub 127.0.0.1 at IPs[0] would break Service endpoints for exposed devices.
	base := &cniv1.Result{
		Interfaces: []*cniv1.Interface{{Name: "accessport", Sandbox: "/ns/1"}},
		IPs:        []*cniv1.IPConfig{ipConfig(0, "10.244.0.5/32")},
	}
	got := ensureEth0(base, "/ns/1")
	if len(got.Interfaces) != 2 || got.Interfaces[0].Name != "eth0" {
		t.Fatalf("eth0 must be prepended: %+v", got.Interfaces)
	}
	if got.Interfaces[0].Sandbox != "/ns/1" {
		t.Fatalf("eth0 sandbox must be the pod netns, got %q", got.Interfaces[0].Sandbox)
	}
	if len(got.IPs) != 1 {
		t.Fatalf("no stub IP must be added when a real IP exists: %+v", got.IPs)
	}
	if *got.IPs[0].Interface != 0 || got.IPs[0].Address.IP.String() != "10.244.0.5" {
		t.Fatalf("real IP must be redirected to eth0 (index 0): %+v", got.IPs[0])
	}
}

func TestEnsureEth0_AbsentMultipleIPs(t *testing.T) {
	base := &cniv1.Result{
		Interfaces: []*cniv1.Interface{
			{Name: "accessport", Sandbox: "/ns/1"},
			{Name: "extra", Sandbox: "/ns/1"},
		},
		IPs: []*cniv1.IPConfig{ipConfig(0, "10.244.0.5/32"), ipConfig(1, "10.244.0.6/32")},
	}
	got := ensureEth0(base, "/ns/1")
	// Primary IP → eth0 (0); secondary keeps pointing at "extra", shifted 1→2.
	if *got.IPs[0].Interface != 0 {
		t.Fatalf("primary IP must point at eth0: %+v", got.IPs[0])
	}
	if *got.IPs[1].Interface != 2 {
		t.Fatalf("secondary IP must shift to index 2: %+v", got.IPs[1])
	}
}

func TestEnsureEth0_AbsentEmptyResult(t *testing.T) {
	// Stub-branch case: device pod with no delegate result at all.
	got := ensureEth0(&cniv1.Result{}, "/ns/1")
	if len(got.Interfaces) != 1 || got.Interfaces[0].Name != "eth0" {
		t.Fatalf("expected lone stub eth0: %+v", got.Interfaces)
	}
	if len(got.IPs) != 1 || *got.IPs[0].Interface != 0 {
		t.Fatalf("expected lone stub IP on eth0: %+v", got.IPs)
	}
}

// fakeDelegate puts a CNI plugin named "faildel" on CNI_PATH that, like Cilium for a pod whose netns is gone, answers every
// command with a CNI error, and leaves a marker file when it is called.
func fakeDelegate(t *testing.T) (marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "called")
	script := "#!/bin/sh\ncat >/dev/null\ntouch " + marker +
		"\necho '{\"cniVersion\":\"1.0.0\",\"code\":999,\"msg\":\"Errors encountered while deleting endpoint\"}'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "faildel"), []byte(script), 0o755); err != nil { //nolint:gosec // an executable stub plugin
		t.Fatal(err)
	}
	t.Setenv("CNI_PATH", dir)
	return marker
}

func delArgs(netns string) *skel.CmdArgs {
	return &skel.CmdArgs{
		ContainerID: "c1",
		Netns:       netns,
		IfName:      "eth0",
		StdinData:   []byte(`{"cniVersion":"1.0.0","name":"gate","type":"cni-gate","delegate":{"type":"faildel"}}`),
	}
}

// DEL of a pod whose netns is gone succeeds even when the delegate fails, and the delegate is still asked to clean up.
func TestDelSucceedsWhenNetnsIsGone(t *testing.T) {
	marker := fakeDelegate(t)
	for _, netns := range []string{"", filepath.Join(t.TempDir(), "removed")} {
		_ = os.Remove(marker)
		if err := cmdDEL(delArgs(netns)); err != nil {
			t.Errorf("netns %q: DEL must succeed once the netns is gone: %v", netns, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("netns %q: the delegate must still be asked to clean up", netns)
		}
	}
}

// With the netns still there a delegate failure is a real one: the kubelet retries.
func TestDelReportsDelegateErrorWhileNetnsExists(t *testing.T) {
	fakeDelegate(t)
	if err := cmdDEL(delArgs(t.TempDir())); err == nil {
		t.Error("a delegate failure with an existing netns must reach the kubelet")
	}
}
