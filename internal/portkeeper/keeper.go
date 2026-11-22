package portkeeper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Identity is a resolved user of the device (ids, groups and home), as the launcher resolves the service user.
type Identity struct {
	UID, GID int
	Groups   []int
	Home     string
}

// System is what the keeper does to the machine that is not portable or that a test replaces: PTYs and the console programs.
type System interface {
	// OpenPTY opens a new PTY pair from the container's own /dev/ptmx: the master, the slave held open by the keeper, and the slave's path.
	OpenPTY() (master, slave *os.File, name string, err error)
	// MakeRaw puts the terminal in raw mode (no echo, no line editing): a serial line carries bytes as they are.
	MakeRaw(tty *os.File) error
	// StartConsole runs argv on the slave as its controlling terminal (a new session, so its own process group) as the user.
	StartConsole(slave string, argv []string, id Identity, env []string) (*Process, error)
}

// Process is a running console program: Wait returns when it ended, Kill ends its whole process group (what it left running too).
type Process struct {
	Wait func() error
	Kill func()
}

// Config is one keeper.
type Config struct {
	Spec Spec
	// SocketDir holds the sockets of the ports (SocketDir by default).
	SocketDir string
	// Root is the file system root the serial links are made under (tests only); empty is "/".
	Root string
	// Owner owns the serial devices (the service user); nil: the owner recorded by the first keeper of this container, else root.
	Owner *Identity
	// StdioMaster is the master of the stdio console, from the launcher (the service has the slave as stdin, stdout and stderr). A keeper
	// started again later has none: that line is gone with the keeper that held it.
	StdioMaster *os.File
	// Env is the environment of the console programs (the container's).
	Env []string
	// Resolve resolves the user of a console command.
	Resolve func(user string) (Identity, error)
	Clock   Clock
	Sys     System
	// Log receives what goes wrong (stderr of the keeper); nil discards it.
	Log io.Writer
}

const (
	lockFile  = ".lock"
	ownerFile = ".owner"
	// failedSuffix names the record of a port the keeper could not make (<port>.failed holds the reason).
	failedSuffix = ".failed"
	// consoleTerm is what a console line announces, like a real serial console.
	consoleTerm = "vt100"
	dialTimeout = 2 * time.Second
)

// ErrRunning: another keeper holds the ports of this container.
var ErrRunning = errors.New("a port keeper already runs")

// Run makes every port of the list and serves it until ctx ends. It returns at once when another keeper runs in the container.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.Spec.Validate(); err != nil {
		return err
	}
	if cfg.SocketDir == "" {
		cfg.SocketDir = SocketDir
	}
	if cfg.Root == "" {
		cfg.Root = "/"
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock{}
	}
	logf := func(format string, a ...any) {
		if cfg.Log != nil {
			_, _ = fmt.Fprintf(cfg.Log, "port keeper: "+format+"\n", a...)
		}
	}
	if err := os.MkdirAll(cfg.SocketDir, 0o700); err != nil {
		return err
	}
	// The directory is root's only: no other user of the device reaches a line.
	if err := os.Chmod(cfg.SocketDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.SocketDir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrRunning
	}
	owner := ownerOf(cfg)

	if cfg.StdioMaster != nil {
		// The stdio master must never reach a console program (another user could read and type into the service's terminal).
		syscall.CloseOnExec(int(cfg.StdioMaster.Fd()))
	}

	var listeners []net.Listener
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	// A port that cannot be made is recorded (<port>.failed, with the reason) for the attach, and the other ports go on.
	failed := func(name string, err error) {
		logf("port %s: %v", name, err)
		_ = os.WriteFile(filepath.Join(cfg.SocketDir, name+failedSuffix), []byte(err.Error()), 0o600)
	}
	serve := func(name string, h *Hub) {
		ln, err := Listen(SocketPath(cfg.SocketDir, name))
		if err != nil {
			failed(name, err)
			return
		}
		_ = os.Remove(filepath.Join(cfg.SocketDir, name+failedSuffix))
		listeners = append(listeners, ln)
		go h.Serve(ln)
	}

	for _, p := range cfg.Spec.Serial {
		h, err := serialPort(cfg, p, owner)
		if err != nil {
			failed(p.Name, err)
			continue
		}
		serve(p.Name, h)
	}
	for _, c := range cfg.Spec.Console {
		h := NewHub()
		switch c.Native {
		case NativeStdio:
			if cfg.StdioMaster == nil {
				failed(c.Name, errors.New("the stdio line belonged to the keeper that started with the service; it is not served again"))
				continue
			}
			h.SetLine(cfg.StdioMaster)
			go func() { _ = h.Pump(cfg.StdioMaster) }()
		case NativeTCP:
			go Supervise(ctx, cfg.Clock, func() error { return tcpLine(ctx, h, c.Port) })
		default:
			if err := manufacturedLine(ctx, cfg, c, h, logf); err != nil {
				failed(c.Name, err)
				continue
			}
		}
		serve(c.Name, h)
	}
	<-ctx.Done()
	return nil
}

// ownerOf is the owner of the serial devices: the one given, else the one the first keeper recorded, else root. The first keeper records
// it, so a keeper started again (by an attach) keeps the service user's ownership.
func ownerOf(cfg Config) Identity {
	path := filepath.Join(cfg.SocketDir, ownerFile)
	if cfg.Owner != nil {
		_ = os.WriteFile(path, []byte(fmt.Sprintf("%d:%d", cfg.Owner.UID, cfg.Owner.GID)), 0o600)
		return *cfg.Owner
	}
	if b, err := os.ReadFile(path); err == nil {
		u, g, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
		uid, err1 := strconv.Atoi(u)
		gid, err2 := strconv.Atoi(g)
		if ok && err1 == nil && err2 == nil {
			return Identity{UID: uid, GID: gid}
		}
	}
	return Identity{}
}

// serialPort makes the PTY of a serial port and links its slave at the declared path, owned by the service user, mode 0660.
func serialPort(cfg Config, p SerialPort, owner Identity) (*Hub, error) {
	master, slave, name, err := cfg.Sys.OpenPTY()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Hub, error) {
		_ = master.Close()
		_ = slave.Close()
		return nil, err
	}
	// The keeper holds the slave open: the master never reads EIO while no program has the device open, and the raw mode stays.
	if err := cfg.Sys.MakeRaw(slave); err != nil {
		return fail(err)
	}
	if err := os.Chown(name, owner.UID, owner.GID); err != nil {
		return fail(err)
	}
	if err := os.Chmod(name, 0o660); err != nil {
		return fail(err)
	}
	link := filepath.Join(cfg.Root, p.Path)
	// /dev is a tmpfs of root in a container: root writes the link without any capability. A link left by an earlier keeper is replaced.
	if st, err := os.Lstat(link); err == nil && st.Mode()&os.ModeSymlink == 0 {
		return fail(fmt.Errorf("%s exists and is not a link of the keeper", p.Path))
	}
	_ = os.Remove(link)
	if err := os.Symlink(name, link); err != nil {
		return fail(err)
	}
	h := NewHub()
	h.SetLine(master)
	go func() {
		_ = h.Pump(master)
		_ = slave.Close()
	}()
	return h, nil
}

// manufacturedLine makes the PTY of a console port and keeps the console program running on it.
func manufacturedLine(ctx context.Context, cfg Config, c ConsolePort, h *Hub, logf func(string, ...any)) error {
	master, slave, name, err := cfg.Sys.OpenPTY()
	if err != nil {
		return err
	}
	id, err := cfg.Resolve(c.EffectiveUser())
	if err != nil {
		_ = master.Close()
		_ = slave.Close()
		return err
	}
	env := consoleEnv(cfg.Env, id)
	argv := c.EffectiveCommand()
	h.SetLine(master)
	go func() {
		// The keeper holds the slave open for the life of the line: the line and its settings survive a respawn of the program.
		_ = h.Pump(master)
		_ = slave.Close()
	}()
	go Supervise(ctx, cfg.Clock, func() error {
		p, err := cfg.Sys.StartConsole(name, argv, id, env)
		if err != nil {
			logf("console port %s: the program cannot be started: %v", c.Name, err)
			return err
		}
		// The keeper stopping ends the program and its process group. A program that ends by itself (a logout) is only started again:
		// what it left running stays, as on a real machine, and the keeper (the subreaper) reaps it when it ends.
		stop := context.AfterFunc(ctx, p.Kill)
		err = p.Wait()
		stop()
		return err
	})
	return nil
}

// consoleEnv is the container's environment with TERM of a serial console and the HOME of the console user.
func consoleEnv(base []string, id Identity) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if strings.HasPrefix(kv, "TERM=") || (id.UID != 0 && strings.HasPrefix(kv, "HOME=")) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TERM="+consoleTerm)
	if id.UID != 0 {
		home := id.Home
		if home == "" {
			home = "/"
		}
		out = append(out, "HOME="+home)
	}
	return out
}

// tcpLine dials a tcp console on the device's own loopback and bridges it until it ends. Loopback never leaves the device: it is not the
// lab network. It is not private inside the device either: any process of the device can connect to that port too (the emulator's own
// "telnet to the console"), so a tcp console is as confidential as the device itself, no more.
func tcpLine(ctx context.Context, h *Hub, port int) error {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	h.SetLine(conn)
	defer h.SetLine(nil)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	return h.Pump(conn)
}
