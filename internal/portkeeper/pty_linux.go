//go:build linux

package portkeeper

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OS is the real System.
type OS struct{}

// OpenPTY opens a PTY pair from the container's own /dev/ptmx (runc and Kata give each container its own devpts instance). No mknod, no
// device cgroup rule, no capability.
func (OS) OpenPTY() (master, slave *os.File, name string, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, "", err
	}
	var n int
	var ierr error
	if cerr := control(master, func(fd int) {
		if ierr = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); ierr != nil {
			return
		}
		n, ierr = unix.IoctlGetInt(fd, unix.TIOCGPTN)
	}); cerr != nil || ierr != nil {
		_ = master.Close()
		if cerr != nil {
			return nil, nil, "", cerr
		}
		return nil, nil, "", ierr
	}
	name = fmt.Sprintf("/dev/pts/%d", n)
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, "", err
	}
	return master, slave, name, nil
}

// MakeRaw is cfmakeraw(3) on the terminal.
func (OS) MakeRaw(tty *os.File) error {
	var ierr error
	if err := control(tty, func(fd int) {
		t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			ierr = err
			return
		}
		t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		t.Oflag &^= unix.OPOST
		t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		t.Cflag &^= unix.CSIZE | unix.PARENB
		t.Cflag |= unix.CS8
		t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
		ierr = unix.IoctlSetTermios(fd, unix.TCSETS, t)
	}); err != nil {
		return err
	}
	return ierr
}

// StartConsole runs the program with the slave as stdin, stdout, stderr and controlling terminal of a new session (so a process group of
// its own), as the user. The keeper's reaper collects it; Kill ends the whole group.
func (OS) StartConsole(slave string, argv []string, id Identity, env []string) (*Process, error) {
	tty, err := os.OpenFile(slave, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tty.Close() }()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = "/"
	if id.Home != "" {
		if st, err := os.Stat(id.Home); err == nil && st.IsDir() {
			cmd.Dir = id.Home
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	// The keeper is root; the program runs as the console user. A keeper that already is that user (a test) does not switch.
	if id.UID != os.Geteuid() || id.GID != os.Getegid() || len(id.Groups) > 0 {
		groups := make([]uint32, 0, len(id.Groups))
		for _, g := range id.Groups {
			groups = append(groups, uint32(g)) //nolint:gosec // group ids of /etc/group
		}
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(id.UID), Gid: uint32(id.GID), Groups: groups} //nolint:gosec // ids of /etc/passwd
	}
	reaper.once.Do(startReaper)
	// The reaper must not collect the program before it is registered: it waits for this lock.
	reaper.mu.Lock()
	if err := cmd.Start(); err != nil {
		reaper.mu.Unlock()
		return nil, err
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	reaper.waiters[pid] = done
	reaper.mu.Unlock()
	return &Process{
		Wait: func() error {
			<-done
			_ = cmd.Process.Release()
			return nil
		},
		Kill: func() { _ = unix.Kill(-pid, unix.SIGKILL) },
	}, nil
}

// reaper collects every child of the keeper: the console programs (handed to their Wait) and, with PR_SET_CHILD_SUBREAPER, whatever
// they left behind, so no zombie stays in the device.
var reaper struct {
	once    sync.Once
	mu      sync.Mutex
	waiters map[int]chan struct{}
}

func startReaper() {
	reaper.waiters = map[int]chan struct{}{}
	go func() {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, 0, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil { // no child right now
				time.Sleep(200 * time.Millisecond)
				continue
			}
			reaper.mu.Lock()
			if ch, ok := reaper.waiters[pid]; ok {
				close(ch)
				delete(reaper.waiters, pid)
			}
			reaper.mu.Unlock()
		}
	}()
}

// BecomeSubreaper makes the keeper the reaper of the orphans of its console programs (a daemon started from a console shell): they are
// re-parented to the keeper, not to the service (process 1 of the container, which may never reap them).
func BecomeSubreaper() error { return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) }

func control(f *os.File, fn func(fd int)) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	return rc.Control(func(fd uintptr) { fn(int(fd)) }) //nolint:gosec // a file descriptor fits an int
}
