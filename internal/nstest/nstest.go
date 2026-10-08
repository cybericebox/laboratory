//go:build linux

// Package nstest builds small network topologies out of Linux network namespaces for tests of real iptables rules.
//
// The test process is the pod: it runs in a network namespace of its own (`unshare -n`, see the Makefile target test-netns), and
// its rules are installed with the real iptables binary. The other sides (a lab device, a participant, the pod network) are
// namespaces made with `ip netns`; probes in them run as the test binary itself (the helper process pattern), so the tests need
// no netcat. The namespace of the pod is called "" in every function here.
package nstest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const helperEnv = "CICE_NSTEST_HELPER"

// Require skips the test unless it runs where it may build namespaces: opted in with CICE_NETNS_TESTS=1, as root, with ip and
// iptables, in a network namespace that holds nothing but the loopback (and down fallback devices) (so that the rules of the host are never touched).
func Require(t *testing.T) {
	t.Helper()
	if os.Getenv("CICE_NETNS_TESTS") != "1" {
		t.Skip("set CICE_NETNS_TESTS=1 (make test-netns) to run the real-iptables namespace tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	for _, bin := range []string{"ip", "iptables", "ping"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s", bin)
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		// A fresh namespace may hold the fallback tunnel devices of loaded modules (tunl0, sit0, ...): they are down.
		if i.Name != "lo" && i.Flags&net.FlagUp != 0 {
			t.Fatalf("the test would change the rules of a real network namespace (found %s up): run it with `unshare -n`", i.Name)
		}
	}
	Run(t, "", "ip", "link", "set", "lo", "up")
}

// Run runs a command in a namespace and fails the test when it fails.
func Run(t *testing.T, ns string, argv ...string) string {
	t.Helper()
	out, err := Try(ns, argv...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	return out
}

// Try runs a command in a namespace and returns its error.
func Try(ns string, argv ...string) (string, error) {
	if ns != "" {
		argv = append([]string{"ip", "netns", "exec", ns}, argv...)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

// NS creates a namespace, removed at the end of the test.
func NS(t *testing.T, name string) {
	t.Helper()
	Run(t, "", "ip", "netns", "add", name)
	t.Cleanup(func() { _, _ = Try("", "ip", "netns", "del", name) })
	Run(t, name, "ip", "link", "set", "lo", "up")
}

// Veth joins two namespaces with a cable: the end a stays in nsA, the end b goes to nsB. Both ends are up, addressed with the
// given CIDRs (empty = none).
func Veth(t *testing.T, nsA, a, cidrA, nsB, b, cidrB string) {
	t.Helper()
	Run(t, "", "ip", "link", "add", a, "type", "veth", "peer", "name", b)
	if nsA != "" {
		Run(t, "", "ip", "link", "set", a, "netns", nsA)
	}
	if nsB != "" {
		Run(t, "", "ip", "link", "set", b, "netns", nsB)
	}
	for _, e := range []struct{ ns, dev, cidr string }{{nsA, a, cidrA}, {nsB, b, cidrB}} {
		if e.cidr != "" {
			Run(t, e.ns, "ip", "addr", "add", e.cidr, "dev", e.dev)
		}
		Run(t, e.ns, "ip", "link", "set", e.dev, "up")
	}
}

// Ping says whether one ICMP echo to dst gets an answer.
func Ping(t *testing.T, ns, dst string) bool {
	t.Helper()
	_, err := Try(ns, "ping", "-c", "1", "-W", "1", dst)
	return err == nil
}

// Listener is a TCP or UDP listener running in a namespace.
type Listener struct {
	hits chan struct{}
}

// Listen starts a listener (a TCP accept or a UDP datagram counts as a hit).
func Listen(t *testing.T, ns, proto, addr string) *Listener {
	t.Helper()
	argv := helperArgv("listen", proto, addr)
	if ns != "" {
		argv = append([]string{"ip", "netns", "exec", ns}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	l := &Listener{hits: make(chan struct{}, 64)}
	sc := bufio.NewScanner(out)
	if !sc.Scan() || sc.Text() != "ready" {
		t.Fatalf("listener %s %s in %q did not start: %q", proto, addr, ns, sc.Text())
	}
	go func() {
		for sc.Scan() {
			l.hits <- struct{}{}
		}
	}()
	return l
}

// Hit says whether the listener was reached within a short time; each call consumes the hits so far.
func (l *Listener) Hit() bool {
	select {
	case <-l.hits:
		for {
			select {
			case <-l.hits:
			default:
				return true
			}
		}
	case <-time.After(700 * time.Millisecond):
		return false
	}
}

// Dial connects (tcp) or sends a datagram (udp) from a namespace; the error says whether the connection was made.
func Dial(t *testing.T, ns, proto, addr string) bool {
	t.Helper()
	argv := helperArgv("dial", proto, addr)
	if ns != "" {
		argv = append([]string{"ip", "netns", "exec", ns}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	return cmd.Run() == nil
}

// Reach is Dial plus the check that the listener was hit: a TCP connection that was accepted, a datagram that arrived.
func Reach(t *testing.T, from, proto, addr string, l *Listener) bool {
	t.Helper()
	ok := Dial(t, from, proto, addr)
	hit := l.Hit()
	if proto == "tcp" {
		return ok && hit
	}
	return hit
}

func helperArgv(mode, proto, addr string) []string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return []string{exe, "-test.run=^TestNetnsHelperProcess$", "--", mode, proto, addr}
}

// HelperProcess is the body of the probes that run in the namespaces. Every test package that uses nstest has
//
//	func TestNetnsHelperProcess(t *testing.T) { nstest.HelperProcess() }
func HelperProcess() {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) != 3 {
		os.Exit(2)
	}
	mode, proto, addr := args[0], args[1], args[2]
	switch mode + " " + proto {
	case "listen tcp":
		l, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ready")
		for {
			c, err := l.Accept()
			if err != nil {
				os.Exit(1)
			}
			_ = c.Close()
			fmt.Println("hit")
		}
	case "listen udp":
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ready")
		buf := make([]byte, 64)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				os.Exit(1)
			}
			fmt.Println("hit")
		}
	case "echo tcp", "echo udp", "persist tcp", "persist udp":
		helperPersistent(mode, proto, addr)
	case "dial dhcp":
		helperDHCP(addr)
	case "dial tcp":
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			os.Exit(1)
		}
		_ = c.Close()
		os.Exit(0)
	case "dial udp":
		c, err := net.Dial("udp", addr)
		if err != nil {
			os.Exit(1)
		}
		_, _ = c.Write([]byte("x"))
		_ = c.Close()
		os.Exit(0)
	}
	os.Exit(2)
}
