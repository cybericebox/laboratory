package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// System is everything the launcher does to the machine that a test must not do for real: switching users, replacing the
// process, waiting. The real implementation is OS.
type System interface {
	Environ() []string
	Chown(path string, uid, gid int) error
	// Drop makes the process the given user: supplementary groups, group, user, in that order.
	Drop(id Identity) error
	// Exec replaces the process; it returns only when that failed.
	Exec(path string, argv, env []string) error
	// RunScript runs the script with its interpreter and waits. The script text is passed through a pipe, never on a
	// command line or in a file. Output is discarded: it could carry values.
	RunScript(ctx context.Context, interpreter, script string, env []string) error
	Sleep(d time.Duration)
	// Pause blocks until the container is told to stop (the device of a failed bootstrap waits, it is not restarted).
	Pause()
}

// ErrTimeout: the script did not finish within the time limit.
var ErrTimeout = errors.New("timed out")

// ScriptExitError: the script ended with a non-zero exit code (or was killed by a signal, Code -1).
type ScriptExitError struct{ Code int }

func (e *ScriptExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

// StartError: the interpreter could not be started.
type StartError struct{ Err error }

func (e *StartError) Error() string { return "cannot start the interpreter" }
func (e *StartError) Unwrap() error { return e.Err }

// OS is the real System.
type OS struct{}

func (OS) Environ() []string                          { return os.Environ() }
func (OS) Chown(path string, uid, gid int) error      { return os.Chown(path, uid, gid) }
func (OS) Sleep(d time.Duration)                      { time.Sleep(d) }
func (OS) Exec(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }

func (OS) Drop(id Identity) error {
	if err := syscall.Setgroups(id.Groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(id.GID); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(id.UID); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}

func (OS) Pause() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	<-c
}

func (OS) RunScript(ctx context.Context, interpreter, script string, env []string) error {
	r, w, err := os.Pipe()
	if err != nil {
		return &StartError{Err: err}
	}
	defer func() { _ = r.Close() }()
	// The interpreter reads the script from descriptor 3 (file descriptors 0-2 are /dev/null).
	cmd := exec.CommandContext(ctx, interpreter, "/dev/fd/3")
	cmd.ExtraFiles = []*os.File{r}
	cmd.Env = env
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The whole process group dies with the script: a daemon it forked must not outlive a failed attempt.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return &StartError{Err: err}
	}
	go func() {
		_, _ = io.WriteString(w, script)
		_ = w.Close()
	}()
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ErrTimeout
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ScriptExitError{Code: ee.ExitCode()}
	}
	return err
}
