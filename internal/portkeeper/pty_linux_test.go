//go:build linux

package portkeeper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real keeper on Linux (design section 11, "keeper integration test"): /dev/ttyCON0 opens as a terminal and its bytes reach the
// attach socket and back; a console port runs its program on a PTY and respawns it; one client per line.
func TestKeeperOnRealPTYs(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	sockets := shortDir(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	me := Identity{UID: os.Getuid(), GID: os.Getgid()}
	spec := Spec{
		Console: []ConsolePort{{Name: "console0", Command: []string{"/bin/cat"}}, {Name: "aux0", Command: []string{"/bin/echo", "boot"}}},
		Serial:  []SerialPort{{Name: "serial0", Path: "/dev/ttyCON0"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, Config{Spec: spec, SocketDir: sockets, Root: root, Owner: &me, Env: os.Environ(), Sys: OS{},
			Resolve: func(string) (Identity, error) { return me, nil }})
	}()
	for _, n := range spec.Names() {
		sock := SocketPath(sockets, n)
		eventually(t, "socket "+n, func() bool { _, err := os.Stat(sock); return err == nil })
	}

	// serial: a program opens /dev/ttyCON0 like a serial device
	tty, err := os.OpenFile(filepath.Join(root, "dev/ttyCON0"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	st, _ := os.Stat(filepath.Join(root, "dev/ttyCON0"))
	if st.Mode()&os.ModeCharDevice == 0 || st.Mode().Perm() != 0o660 {
		t.Fatalf("ttyCON0 is %v", st.Mode())
	}
	c := dialAttached(t, SocketPath(sockets, "serial0"))
	defer func() { _ = c.Close() }()
	if _, err := tty.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("serial to line: %q %v (raw mode, no echo, no translation)", got, err)
	}
	if _, err := c.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	back := make([]byte, 4)
	if _, err := io.ReadFull(tty, back); err != nil || string(back) != "pong" {
		t.Fatalf("line to serial: %q %v", back, err)
	}

	// console: cat on the PTY answers what is typed (the line discipline echoes and cat repeats it)
	cc := dialAttached(t, SocketPath(sockets, "console0"))
	defer func() { _ = cc.Close() }()
	if _, err := cc.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	buf := make([]byte, 256)
	_ = cc.SetReadDeadline(time.Now().Add(3 * time.Second))
	for strings.Count(out.String(), "hello") < 2 {
		n, err := cc.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			t.Fatalf("console output %q: %v", out.String(), err)
		}
	}

	// respawn: a program that ends at once comes back (after 1 s, then 2 s)
	ac := dialAttached(t, SocketPath(sockets, "aux0"))
	defer func() { _ = ac.Close() }()
	out.Reset()
	_ = ac.SetReadDeadline(time.Now().Add(6 * time.Second))
	for strings.Count(out.String(), "boot") < 2 {
		n, err := ac.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			t.Fatalf("respawned output %q: %v", out.String(), err)
		}
	}
}
