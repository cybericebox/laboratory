package portkeeper

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeSys hands out socket pairs as PTYs (both ends read and write, like a master and its slave) and records the console programs.
type fakeSys struct {
	t      *testing.T
	dir    string
	mu     sync.Mutex
	peers  map[string]*os.File // slave name -> the far end of the master (what a program on the slave would see)
	starts [][]string
	envs   [][]string
	users  []Identity
	n      int
}

func pair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
}

func (f *fakeSys) OpenPTY() (master, slave *os.File, name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, s := pair(f.t)
	f.n++
	name = filepath.Join(f.dir, "pts"+strconv.Itoa(f.n))
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		return nil, nil, "", err
	}
	f.peers[name] = s
	_, slaveHeld := pair(f.t)
	return m, slaveHeld, name, nil
}
func (f *fakeSys) MakeRaw(*os.File) error { return nil }
func (f *fakeSys) StartConsole(slave string, argv []string, id Identity, env []string) (func() error, error) {
	f.mu.Lock()
	f.starts = append(f.starts, argv)
	f.envs = append(f.envs, env)
	f.users = append(f.users, id)
	f.mu.Unlock()
	block := make(chan struct{})
	return func() error { <-block; return nil }, nil
}
func (f *fakeSys) peer(name string) *os.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peers[name]
}

func startKeeper(t *testing.T, spec Spec) (*fakeSys, string, string, context.CancelFunc) {
	t.Helper()
	sockets := shortDir(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	sys := &fakeSys{t: t, dir: t.TempDir(), peers: map[string]*os.File{}}
	ctx, cancel := context.WithCancel(context.Background())
	me := Identity{UID: os.Getuid(), GID: os.Getgid()}
	cfg := Config{Spec: spec, SocketDir: sockets, Root: root, Owner: &me, Env: []string{"PATH=/bin", "TERM=xterm"},
		Resolve: func(user string) (Identity, error) {
			if user != "root" && user != "admin" {
				return Identity{}, errors.New("unknown user")
			}
			return Identity{UID: 0, GID: 0, Home: "/root"}, nil
		}, Sys: sys}
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
		}
	})
	for _, n := range spec.Names() {
		sock := SocketPath(sockets, n)
		eventually(t, "socket "+n, func() bool { _, err := os.Stat(sock); return err == nil })
	}
	return sys, sockets, root, cancel
}

// The keeper links each serial port at its path (to the PTY slave, owned by the service user, mode 0660), runs /bin/login as root on a
// console port by default with TERM=vt100, serves every port on its socket, and a second keeper in the same container refuses to run.
func TestKeeperMakesThePorts(t *testing.T) {
	spec := Spec{Console: []ConsolePort{{Name: "console0"}}, Serial: []SerialPort{{Name: "serial0", Path: "/dev/ttyCON0"}}}
	sys, sockets, root, _ := startKeeper(t, spec)

	target, err := os.Readlink(filepath.Join(root, "dev/ttyCON0"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(target)
	if err != nil || st.Mode().Perm() != 0o660 {
		t.Fatalf("slave %s: %v %v", target, st, err)
	}
	if b, err := os.ReadFile(filepath.Join(sockets, ownerFile)); err != nil || string(b) != strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()) {
		t.Fatalf("owner record %q %v", b, err)
	}
	if st, err := os.Stat(sockets); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v %v", st, err)
	}
	if st, err := os.Stat(SocketPath(sockets, "serial0")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", st, err)
	}
	eventually(t, "the console program", func() bool { sys.mu.Lock(); defer sys.mu.Unlock(); return len(sys.starts) == 1 })
	sys.mu.Lock()
	if !slices.Equal(sys.starts[0], []string{"/bin/login"}) || !slices.Contains(sys.envs[0], "TERM=vt100") || slices.Contains(sys.envs[0], "TERM=xterm") {
		t.Fatalf("console start %v env %v", sys.starts, sys.envs)
	}
	sys.mu.Unlock()

	// a byte the serial user writes reaches the attached client, and back
	c := dialAttached(t, SocketPath(sockets, "serial0"))
	defer func() { _ = c.Close() }()
	far := sys.peer(target)
	if _, err := far.Write([]byte("AT\r")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 3)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "AT\r" {
		t.Fatalf("serial to client: %q %v", got, err)
	}
	if _, err := c.Write([]byte("OK")); err != nil {
		t.Fatal(err)
	}
	back := make([]byte, 2)
	_ = far.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(far, back); err != nil || string(back) != "OK" {
		t.Fatalf("client to serial: %q %v", back, err)
	}

	// a second keeper of the same container does not run
	err = Run(context.Background(), Config{Spec: spec, SocketDir: sockets, Sys: sys, Resolve: func(string) (Identity, error) { return Identity{}, nil }})
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("second keeper: %v", err)
	}
}

// A tcp console is bridged to a port on the device's loopback, and dialled again when it ends.
func TestKeeperBridgesATCPConsole(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	_, sockets, _, _ := startKeeper(t, Spec{Console: []ConsolePort{{Name: "console1", Native: NativeTCP, Port: port}}})
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c := dialAttached(t, SocketPath(sockets, "console1"))
	defer func() { _ = c.Close() }()
	if _, err := srv.Write([]byte("Router>")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "Router>" {
		t.Fatalf("tcp to client: %q %v", got, err)
	}
	if _, err := c.Write([]byte("en\n")); err != nil {
		t.Fatal(err)
	}
	in := make([]byte, 3)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(srv, in); err != nil || string(in) != "en\n" {
		t.Fatalf("client to tcp: %q %v", in, err)
	}
	// the emulator restarts its console: the keeper dials again (after the first respawn delay)
	_ = srv.Close()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	srv2, err := ln.Accept()
	if err != nil {
		t.Fatalf("no redial: %v", err)
	}
	_ = srv2.Close()
}

// A console user that cannot be resolved leaves that port out; the others still run.
func TestKeeperSkipsAPortItCannotMake(t *testing.T) {
	sys, sockets, _, _ := startKeeper(t, Spec{Console: []ConsolePort{{Name: "aux0"}}, Serial: []SerialPort{{Name: "serial1", Path: "/dev/ttyCON1"}}})
	_ = sys
	if _, err := os.Stat(SocketPath(sockets, "aux0")); err != nil {
		t.Fatal(err)
	}
	bad := Spec{Console: []ConsolePort{{Name: "console0", User: "nobody-here"}}}
	if err := bad.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(consoleEnv([]string{"HOME=/root"}, Identity{UID: 1000, Home: "/home/a"})[1], "HOME=/home/a") {
		t.Fatal("a non-root console user gets its own HOME")
	}
}
