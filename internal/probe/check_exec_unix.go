//go:build unix

package probe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// capBuffer keeps the first limit bytes written to it and counts the rest as dropped (a command that writes without end must not
// grow the memory of the device, nor the answer).
type capBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	limit   int
	dropped bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - c.buf.Len()
	if room > 0 {
		n := min(room, len(p))
		c.buf.Write(p[:n])
	}
	if len(p) > room {
		c.dropped = true
	}
	return len(p), nil
}

// credentialFor resolves the user of a check: a number (uid or uid:gid) or a name of the device's /etc/passwd.
// account is a user of the device.
type account struct {
	uid, gid uint32
	name     string
	home     string
}

// nobodyID is the user "nobody" has when the image's /etc/passwd does not name it.
const nobodyID = 65534

// resolveUser finds a user in the passwd file of etc: a name, a number, or a number and a group ("1000:1000"). A number without a
// passwd line is a user too and has the group of its own number. The name "nobody" exists even in an image that has no such line.
func resolveUser(etc, user string) (account, error) {
	name, group, hasGroup := strings.Cut(user, ":")
	lines := strings.Split(string(readBounded(filepath.Join(etc, "passwd"), MaxIDFile)), "\n")
	byName := func(match func(f []string) bool) (account, bool) {
		for _, line := range lines {
			f := strings.Split(line, ":")
			if len(f) < 6 || !match(f) {
				continue
			}
			u, e1 := strconv.ParseUint(f[2], 10, 32)
			g, e2 := strconv.ParseUint(f[3], 10, 32)
			if e1 == nil && e2 == nil {
				return account{uid: uint32(u), gid: uint32(g), name: f[0], home: f[5]}, true
			}
		}
		return account{}, false
	}
	var a account
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		var ok bool
		if a, ok = byName(func(f []string) bool { return f[2] == name }); !ok {
			a = account{uid: uint32(n), gid: uint32(n), home: "/"}
		}
	} else {
		var ok bool
		if a, ok = byName(func(f []string) bool { return f[0] == name }); !ok {
			if name != "nobody" {
				return account{}, errors.New("no such user")
			}
			a = account{uid: nobodyID, gid: nobodyID, name: "nobody", home: "/"}
		}
	}
	if hasGroup {
		g, err := strconv.ParseUint(group, 10, 32)
		if err != nil {
			return account{}, errors.New("bad group in the user")
		}
		a.gid = uint32(g)
	}
	return a, nil
}

// credentialFor is the credential to start a command with, or nil when the command runs as the tool does. It is not nil whenever the
// user differs; starting then needs the tool to be root (the device access service starts it as root).
func credentialFor(user string) (*syscall.Credential, account, error) {
	if user == "" {
		return nil, account{name: "", home: "/"}, nil
	}
	a, err := resolveUser("/etc", user)
	if err != nil {
		return nil, a, err
	}
	if int(a.uid) == os.Geteuid() && int(a.gid) == os.Getegid() {
		return nil, a, nil
	}
	return &syscall.Credential{Uid: a.uid, Gid: a.gid}, a, nil
}

// checkPath is the PATH of a command of a check: fixed, whatever the image says.
const checkPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// cleanEnv is all a command of a check sees of the environment: no variable of the image or of the container.
func cleanEnv(a account) []string {
	home := a.home
	if home == "" {
		home = "/"
	}
	env := []string{"PATH=" + checkPath, "HOME=" + home, "LC_ALL=C"}
	if a.name != "" {
		env = append(env, "USER="+a.name, "LOGNAME="+a.name)
	}
	return env
}

func runCheckExec(req ExecRequest) ExecResponse {
	// An empty user is the user the tool runs as; the checker always names one (nobody unless the author chose another).
	cred, acct, err := credentialFor(req.User)
	if err != nil {
		return ExecResponse{Error: err.Error()}
	}
	if cred != nil && os.Geteuid() != 0 {
		// The runtime started the tool as the user of the image. Until the device access service starts it as root, a command can
		// run only as that user.
		return ExecResponse{Error: "the in-device tool does not run as root, so it cannot switch to the user of the check"}
	}
	argv := req.Argv
	var extra []*os.File
	if req.Script != "" {
		held, err := holdScript(req.Script)
		if err != nil {
			return ExecResponse{Error: "cannot hold the script: " + errnoName(err)}
		}
		defer held.release()
		extra = held.extra
		argv = []string{req.Interpreter, held.path}
	}
	if len(argv) == 0 || argv[0] == "" {
		return ExecResponse{Error: "empty command"}
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The command is looked up in the fixed PATH too, not in the PATH of the image.
	path, err := lookPath(argv[0], checkPath)
	if err != nil {
		return ExecResponse{Error: "cannot start: no such command"}
	}
	cmd := exec.Command(path, argv[1:]...) //nolint:gosec // running the author's command is the purpose
	cmd.Args = argv
	out := &capBuffer{limit: MaxCheckOutput}
	cmd.Stdout, cmd.Stderr, cmd.ExtraFiles = out, out, extra
	cmd.Env, cmd.Dir = cleanEnv(acct), "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	if err := cmd.Start(); err != nil {
		return ExecResponse{Error: "cannot start: " + startError(err)}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	resp := ExecResponse{Started: true}
	select {
	case <-done:
	case <-ctx.Done():
		resp.TimedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	st := cmd.ProcessState
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		resp.Signal = ws.Signal().String()
		resp.Exit = -1
	} else {
		resp.Exit = st.ExitCode()
	}
	out.mu.Lock()
	resp.Output, resp.Truncated = bytes.Clone(out.buf.Bytes()), out.dropped
	out.mu.Unlock()
	return resp
}

// lookPath finds an executable like exec.LookPath, in the given list of directories.
func lookPath(name, dirs string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range strings.Split(dirs, ":") {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

func startError(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "no such command"
	case errors.As(err, &errno):
		return errno.Error()
	}
	return "error"
}
