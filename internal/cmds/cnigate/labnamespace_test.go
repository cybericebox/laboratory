package cnigate

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
)

func TestLabNamespace(t *testing.T) {
	for ns, want := range map[string]bool{
		"lg-event-0123456789ab": true,
		"lg-":                   true,
		"kube-system":           false,
		"laboratory-system":     false,
		"":                      false,
		"xlg-event":             false,
		"LG-event":              false,
	} {
		if got := labNamespace(ns); got != want {
			t.Errorf("labNamespace(%q) = %v, want %v", ns, got, want)
		}
	}
}

// gateFixture installs a fake delegate that logs every call (verb) to a file and answers an ADD with an IP, and points the gate at a
// node-agent socket that does not exist (the node-agent is down).
func gateFixture(t *testing.T) (calls string, stdin []byte) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\ncat >/dev/null\necho \"$CNI_COMMAND\" >> " + calls + "\n" +
		"if [ \"$CNI_COMMAND\" = ADD ]; then echo '{\"cniVersion\":\"1.0.0\",\"interfaces\":[{\"name\":\"eth0\",\"sandbox\":\"/ns\"}],\"ips\":[{\"address\":\"10.0.0.5/24\",\"interface\":0}]}'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "fake-cni"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CNI_PATH", dir)
	stdin = []byte(`{"cniVersion":"1.0.0","name":"gate-test","type":"cni-gate","agentSocket":"` + filepath.Join(dir, "no-such.sock") + `","delegate":{"type":"fake-cni"}}`)
	return calls, stdin
}

func callsOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// runADD runs cmdADD with its stdout (the CNI result) captured.
func runADD(t *testing.T, args *skel.CmdArgs) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := cmdADD(args)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func TestADDOfAnotherNamespaceGoesToTheRealCNIWithoutTheNodeAgent(t *testing.T) {
	calls, stdin := gateFixture(t)
	args := &skel.CmdArgs{ContainerID: "c1", Netns: "/ns", IfName: "eth0", Args: "K8S_POD_NAMESPACE=kube-system;K8S_POD_NAME=coredns;K8S_POD_UID=u1", StdinData: stdin}
	out, err := runADD(t, args)
	if err != nil {
		t.Fatalf("a system pod must start with the node-agent down: %v", err)
	}
	if got := callsOf(t, calls); got != "ADD" {
		t.Fatalf("the real CNI must be called once: %q", got)
	}
	if !strings.Contains(out, "10.0.0.5") {
		t.Fatalf("the result of the real CNI must be returned: %s", out)
	}
}

func TestADDOfALabNamespaceIsRefusedWhenTheNodeAgentIsDown(t *testing.T) {
	calls, stdin := gateFixture(t)
	args := &skel.CmdArgs{ContainerID: "c1", Netns: "/ns", IfName: "eth0", Args: "K8S_POD_NAMESPACE=lg-event-0123456789ab;K8S_POD_NAME=dev;K8S_POD_UID=u1", StdinData: stdin}
	if _, err := runADD(t, args); err == nil {
		t.Fatal("a lab pod must not be wired while the node-agent is down")
	}
	if got := callsOf(t, calls); got != "" {
		t.Fatalf("a lab pod must never reach the real CNI on its own: %q", got)
	}
}

func TestDELOfAnotherNamespaceNeverAsksTheNodeAgent(t *testing.T) {
	calls, stdin := gateFixture(t)
	args := &skel.CmdArgs{ContainerID: "c1", Netns: t.TempDir(), IfName: "eth0", Args: "K8S_POD_NAMESPACE=kube-system;K8S_POD_NAME=coredns;K8S_POD_UID=u1", StdinData: stdin}
	if err := cmdDEL(args); err != nil {
		t.Fatal(err)
	}
	if got := callsOf(t, calls); got != "DEL" {
		t.Fatalf("the real CNI must be told DEL once: %q", got)
	}
}

func TestCHECKDoesNothing(t *testing.T) {
	if err := cmdCHECK(&skel.CmdArgs{}); err != nil {
		t.Fatal(err)
	}
}

// A DEL never waits for the node-agent: with it down, the annotation is unknown, and the real CNI is still told to clean up.
func TestDELOfALabNamespaceWithTheNodeAgentDownStillCleansTheRealCNI(t *testing.T) {
	calls, stdin := gateFixture(t)
	args := &skel.CmdArgs{ContainerID: "c1", Netns: t.TempDir(), IfName: "eth0", Args: "K8S_POD_NAMESPACE=lg-event-0123456789ab;K8S_POD_NAME=dev;K8S_POD_UID=u1", StdinData: stdin}
	if err := cmdDEL(args); err != nil {
		t.Fatal(err)
	}
	if got := callsOf(t, calls); got != "DEL" {
		t.Fatalf("the real CNI must be told DEL once: %q", got)
	}
}
