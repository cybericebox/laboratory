package portkeeper

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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
		cfg.StdioMaster = os.NewFile(uintptr(*stdio), "stdio-console") //nolint:gosec // a descriptor number from the launcher
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
	return Attach(SocketDir, args[0], os.Stdin, os.Stdout, func() error {
		return StartKeeperProcess(KeeperArgs(SpecPath, nil, false), nil)
	})
}

// StartKeeperProcess starts this program as a keeper in a session of its own, with its stdio on /dev/null and extra as descriptors 3...;
// it does not wait for it. Started by the launcher, it outlives the exec of the service (an exec does not end the children; the service,
// process 1, becomes its parent).
func StartKeeperProcess(args []string, extra []*os.File) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, args...)
	cmd.ExtraFiles = extra
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
