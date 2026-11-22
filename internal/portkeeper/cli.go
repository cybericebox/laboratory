package portkeeper

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Commands of the in-device program (cmd/probe).
const (
	// CommandKeeper is "probe ports": the keeper, started by the launcher (or again by an attach).
	CommandKeeper = "ports"
	// CommandAttach is "probe port-attach PORT": what the device access service runs in the device to reach a line.
	CommandAttach = "port-attach"
	// stdioFD is the descriptor the launcher hands the keeper the master of the stdio console on.
	stdioFD = 3
)

// KeeperArgs is the command line of a keeper: the port list, the socket directory, the owner of the serial devices, and whether the
// launcher hands it the stdio master on descriptor 3.
func KeeperArgs(spec string, owner *Identity, stdio bool) []string {
	args := []string{CommandKeeper, "-spec", spec, "-sockets", SocketDir}
	if owner != nil {
		args = append(args, "-owner", fmt.Sprintf("%d:%d", owner.UID, owner.GID))
	}
	if stdio {
		args = append(args, "-stdio-fd", strconv.Itoa(stdioFD))
	}
	return args
}

// KeeperMain is "probe ports -spec F [-sockets D] [-owner UID:GID] [-stdio-fd N]". It runs until the container stops; it returns an
// exit code only when it cannot run.
func KeeperMain(args []string, resolve func(string) (Identity, error), stderr io.Writer) int {
	fs := flag.NewFlagSet(CommandKeeper, flag.ContinueOnError)
	fs.SetOutput(stderr)
	spec := fs.String("spec", SpecPath, "the port list")
	sockets := fs.String("sockets", SocketDir, "the socket directory")
	owner := fs.String("owner", "", "owner of the serial devices, uid:gid")
	stdio := fs.Int("stdio-fd", -1, "descriptor of the stdio console master")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return ExitUsage
	}
	raw, err := os.ReadFile(*spec)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "port keeper: cannot read the port list")
		return 1
	}
	s, err := Parse(raw)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "port keeper:", err)
		return 1
	}
	cfg := Config{Spec: s, SocketDir: *sockets, Env: os.Environ(), Resolve: resolve, Sys: OS{}, Log: stderr}
	if *owner != "" {
		u, g, ok := strings.Cut(*owner, ":")
		uid, err1 := strconv.Atoi(u)
		gid, err2 := strconv.Atoi(g)
		if !ok || err1 != nil || err2 != nil {
			return ExitUsage
		}
		cfg.Owner = &Identity{UID: uid, GID: gid}
	}
	if *stdio >= 0 {
		// Close-on-exec at once: the console programs the keeper starts must never inherit the service's terminal.
		syscall.CloseOnExec(*stdio)
		cfg.StdioMaster = os.NewFile(uintptr(*stdio), "stdio-console") //nolint:gosec // a descriptor number from the launcher
	}
	if err := BecomeSubreaper(); err != nil {
		_, _ = fmt.Fprintln(stderr, "port keeper: not a subreaper:", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := Run(ctx, cfg); err != nil {
		if err == ErrRunning {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "port keeper:", err)
		return 1
	}
	return 0
}

// AttachMain is "probe port-attach PORT".
func AttachMain(args []string) int {
	if len(args) != 1 {
		return ExitUsage
	}
	return attach(SocketDir, args[0], os.Stdin, os.Stdout, os.Stderr, func() error {
		// Not the attach's stderr (a FIFO of the device access service that ends with the attach): keeper.log in the socket directory.
		logFile, err := keeperLog(SocketDir)
		if err != nil {
			logFile = nil
		}
		err = StartKeeperProcess(KeeperArgs(SpecPath, nil, false), nil, logFile)
		if logFile != nil {
			_ = logFile.Close()
		}
		return err
	})
}

// StartKeeperProcess starts this program as a keeper in a session of its own, with stdin and stdout on /dev/null, stderr on the given
// file (the launcher passes the container log; nil is /dev/null) and extra as descriptors 3...; it does not wait for it. Started by the
// launcher, it outlives the exec of the service (an exec does not end the children; the service, process 1, becomes its parent).
func StartKeeperProcess(args []string, extra []*os.File, stderr *os.File) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := keeperCommand(self, args, extra, stderr)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func keeperCommand(self string, args []string, extra []*os.File, stderr *os.File) *exec.Cmd {
	cmd := exec.Command(self, args...)
	cmd.ExtraFiles = extra
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	return cmd
}

const (
	// keeperLogFile is where a keeper started by an attach writes its messages (root only, in the socket directory); keeperLogMax bounds
	// it: a bigger log starts over when the next keeper is started.
	keeperLogFile = "keeper.log"
	keeperLogMax  = 256 << 10
)

func keeperLog(dir string) (*os.File, error) {
	path := filepath.Join(dir, keeperLogFile)
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(path); err == nil && st.Size() > keeperLogMax {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	return f, nil
}
