//go:build unix

package probe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
func credentialFor(user string) (*syscall.Credential, error) {
	if user == "" {
		return nil, nil
	}
	name, group, _ := strings.Cut(user, ":")
	uid, err := strconv.ParseUint(name, 10, 32)
	gid := uint64(0)
	if err != nil {
		uid, gid, err = lookupUser(name)
		if err != nil {
			return nil, err
		}
	} else if group != "" {
		g, gerr := strconv.ParseUint(group, 10, 32)
		if gerr != nil {
			return nil, errors.New("bad group in the user")
		}
		gid = g
	}
	if int(uid) == os.Geteuid() && int(gid) == os.Getegid() {
		return nil, nil
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

func lookupUser(name string) (uid, gid uint64, err error) {
	for _, line := range strings.Split(string(readBounded("/etc/passwd", MaxIDFile)), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] != name {
			continue
		}
		u, e1 := strconv.ParseUint(f[2], 10, 32)
		g, e2 := strconv.ParseUint(f[3], 10, 32)
		if e1 != nil || e2 != nil {
			break
		}
		return u, g, nil
	}
	return 0, 0, errors.New("no such user")
}

func runCheckExec(req ExecRequest) ExecResponse {
	cred, err := credentialFor(req.User)
	if err != nil {
		return ExecResponse{Error: err.Error()}
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
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // running the author's command is the purpose
	out := &capBuffer{limit: MaxCheckOutput}
	cmd.Stdout, cmd.Stderr, cmd.ExtraFiles = out, out, extra
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
