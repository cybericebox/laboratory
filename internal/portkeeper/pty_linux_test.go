//go:build linux

package portkeeper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// processes lists the live processes whose command line holds marker, and the zombies whose parent is this process.
func processes(t *testing.T, marker string) (live, zombies int) {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		if len(fields) > 1 && fields[0] == "Z" && fields[1] == strconv.Itoa(os.Getpid()) {
			zombies++
		}
		cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(cmd), marker) && len(fields) > 0 && fields[0] != "Z" {
			live++
		}
	}
	return live, zombies
}

func runKeeper(t *testing.T, spec Spec, stdio *os.File) (string, context.CancelFunc) {
	t.Helper()
	sockets := shortDir(t)
	me := Identity{UID: os.Getuid(), GID: os.Getgid()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Run(ctx, Config{Spec: spec, SocketDir: sockets, Root: t.TempDir(), Owner: &me, Env: os.Environ(), Sys: OS{}, StdioMaster: stdio,
			Resolve: func(string) (Identity, error) { return me, nil }})
	}()
	for _, n := range spec.Names() {
		sock := SocketPath(sockets, n)
		eventually(t, "socket "+n, func() bool { _, err := os.Stat(sock); return err == nil })
	}
	return sockets, cancel
}

// A console program gets descriptors 0, 1 and 2 only: the stdio master of the service (descriptor 3 from the launcher, inherited without
// close-on-exec) never reaches it.
func TestConsoleProgramGetsNoOtherDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if _, err := unix.FcntlInt(w.Fd(), unix.F_SETFD, 0); err != nil { // as inherited from the launcher
		t.Fatal(err)
	}
	spec := Spec{Console: []ConsolePort{{Name: "console0", Native: NativeStdio}, {Name: "aux0", Command: []string{"/bin/ls", "-l", "/proc/self/fd/"}}}}
	sockets, _ := runKeeper(t, spec, w)
	c := dialAttached(t, SocketPath(sockets, "aux0"))
	defer func() { _ = c.Close() }()
	var out strings.Builder
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
	for strings.Count(out.String(), "->") < 4 {
		n, err := c.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			t.Fatalf("listing %q: %v", out.String(), err)
		}
	}
	listing := out.String()
	if strings.Contains(listing, "pipe:") {
		t.Fatalf("the console program inherited the stdio master:\n%s", listing)
	}
	for _, line := range strings.Split(listing, "\n") {
		if f := strings.Fields(line); len(f) > 2 && f[len(f)-2] == "->" {
			if n, _ := strconv.Atoi(f[len(f)-3]); n > 2 && !strings.HasPrefix(f[len(f)-1], "/proc/") {
				t.Fatalf("descriptor %d reached the console program: %s", n, line)
			}
		}
	}
}

// What a console program leaves running when it ends by itself (a logout) is not killed by the keeper: as on a real machine the kernel
// hangs up the jobs of the terminal, and a daemon (nohup, setsid) stays running; the keeper (subreaper) reaps it when it ends: no zombie. The keeper stopping ends the current program.
func TestConsoleProgramLeftoversStayAndAreReaped(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killMarked(t, "3001") })
	spec := Spec{Console: []ConsolePort{
		{Name: "console0", Command: []string{"/bin/sh", "-c", "nohup sleep 3001 >/dev/null 2>&1 & sleep 0.5; exit 0"}},
		{Name: "aux0", Command: []string{"/bin/sleep", "3002"}},
	}}
	_, cancel := runKeeper(t, spec, nil)
	eventually(t, "the long console program", func() bool { live, _ := processes(t, "3002"); return live == 1 })
	time.Sleep(4500 * time.Millisecond) // two runs of console0 at least (0.5 s each, respawn after 1 s, then 2 s)
	live, zombies := processes(t, "3001")
	if live < 2 || zombies != 0 {
		t.Fatalf("after two logouts: %d left running (want at least 2), %d zombies", live, zombies)
	}
	killMarked(t, "3001") // the daemons end: the keeper reaps them
	eventually(t, "the leftovers to be reaped", func() bool { live, zombies := processes(t, "3001"); return live == 0 && zombies == 0 })
	cancel()
	eventually(t, "the program to end with the keeper", func() bool { live, _ := processes(t, "3002"); return live == 0 })
}

func killMarked(t *testing.T, marker string) {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(cmd), marker) {
			_ = unix.Kill(pid, unix.SIGKILL)
		}
	}
}
