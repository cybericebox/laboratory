//go:build linux

package nstest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

type Dialog struct {
	input io.WriteCloser
	out   *bufio.Reader
}

func Echo(t *testing.T, ns, proto, addr string) { t.Helper(); startTalk(t, ns, "echo", proto, addr) }
func Persistent(t *testing.T, ns, proto, addr string) *Dialog {
	t.Helper()
	return startTalk(t, ns, "persist", proto, addr)
}
func startTalk(t *testing.T, ns, mode, proto, addr string) *Dialog {
	args := helperArgv(mode, proto, addr)
	if ns != "" {
		args = append([]string{"ip", "netns", "exec", ns}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	reader := bufio.NewReader(out)
	line, err := reader.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("persistent peer %q %v", line, err)
	}
	return &Dialog{in, reader}
}
func (d *Dialog) Exchange(t *testing.T) bool {
	t.Helper()
	if _, err := fmt.Fprintln(d.input, "packet"); err != nil {
		t.Fatal(err)
	}
	line, err := d.out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line == "ok\n"
}
func helperPersistent(mode, proto, addr string) {
	if mode == "echo" {
		if proto == "tcp" {
			listener, err := net.Listen("tcp", addr)
			if err != nil {
				os.Exit(2)
			}
			fmt.Println("ready")
			for {
				conn, err := listener.Accept()
				if err != nil {
					os.Exit(2)
				}
				go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
			}
		} else {
			conn, err := net.ListenPacket("udp", addr)
			if err != nil {
				os.Exit(2)
			}
			fmt.Println("ready")
			buf := make([]byte, 1024)
			for {
				n, peer, err := conn.ReadFrom(buf)
				if err != nil {
					os.Exit(2)
				}
				_, _ = conn.WriteTo(buf[:n], peer)
			}
		}
	}
	conn, err := net.DialTimeout(proto, addr, time.Second)
	if err != nil {
		os.Exit(2)
	}
	defer conn.Close()
	fmt.Println("ready")
	scanner := bufio.NewScanner(os.Stdin)
	buf := make([]byte, 1024)
	for scanner.Scan() {
		_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		_, err := conn.Write(scanner.Bytes())
		if err == nil {
			_, err = conn.Read(buf)
		}
		if err == nil {
			fmt.Println("ok")
		} else {
			fmt.Println("blocked")
		}
	}
	os.Exit(0)
}
