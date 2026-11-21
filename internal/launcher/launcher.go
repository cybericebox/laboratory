package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
)

// Names of the files in the launcher's directory (names.BootstrapDir). Only the list and the counters live there; the
// markers hold no data.
const (
	listFile     = labchecks.BootstrapKey
	attemptsFile = "attempts" // the number of the attempt in progress (a pod that restarts in place counts its own attempts)
	finalFile    = "final"    // the last attempt failed: the device waits and is not restarted
	startedFile  = "started"  // the service was started once: a restart means the pod has to be recreated
	okFile       = "ok"
	settledFile  = "settled"
	defaultPath  = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// Config is the command line of the launcher.
type Config struct {
	// Dir is the directory with the list (names.BootstrapDir).
	Dir string
	// Bootstrap: run the list. False: the pod starts from a saved state (or has no list), only the service is started.
	Bootstrap bool
	// Attempt is the number of this attempt when the caller counts them (a pod that is not restarted in place); 0 means the
	// launcher counts them itself in Dir.
	Attempt int
	// User is the service user as the image names it: a name, a uid, name:group or uid:gid. It is required: "root", "0" and "0:0" are the
	// explicit root (a numeric id needs no /etc/passwd, which an image without a shell lacks), and an empty user is refused, never root.
	User string
	// WorkDir is the working directory of the service; empty keeps "/".
	WorkDir string
	// Command is the service: program and arguments.
	Command []string
	// TerminationLog is where the termination message goes; empty is DefaultTerminationLog.
	TerminationLog string
	// Root is the file system root (tests only); empty is "/".
	Root string
}

// Run is the whole launcher. It returns an exit code only when the service did not start; on success it does not return
// (the process becomes the service), unless the System is a fake.
func Run(cfg Config, sys System) int {
	if cfg.TerminationLog == "" {
		cfg.TerminationLog = DefaultTerminationLog
	}
	if cfg.Root == "" {
		cfg.Root = "/"
	}
	if cfg.Dir == "" {
		cfg.Dir = names.BootstrapDir
	}
	l := &run{cfg: cfg, sys: sys, acc: Accounts{Root: cfg.Root}}
	if cfg.User == "" {
		// Before anything is placed or run: the launcher runs as root and would leave the service root.
		Message{Kind: KindStart, Reason: "the service has no user: the user of the image, or root named explicitly"}.write(cfg.TerminationLog)
		return ExitStart
	}
	if cfg.Bootstrap {
		if code, done := l.bootstrap(); done {
			return code
		}
	}
	return l.startService()
}

type run struct {
	cfg Config
	sys System
	acc Accounts
}

func (l *run) file(name string) string { return filepath.Join(l.cfg.Dir, name) }

func (l *run) exists(name string) bool {
	_, err := os.Lstat(l.file(name))
	return err == nil
}

func (l *run) touch(name string) error { return os.WriteFile(l.file(name), nil, 0o644) } //nolint:gosec // a marker the unprivileged probe reads

// bootstrap runs one attempt. done is true when the launcher must return code instead of starting the service.
func (l *run) bootstrap() (code int, done bool) {
	// Markers describe this run only. The directory is traversable for the unprivileged probe and writable by nobody else.
	_ = os.Chmod(l.cfg.Dir, 0o755) //nolint:gosec // the probe runs as the service user and reads the markers
	_ = os.Remove(l.file(okFile))
	_ = os.Remove(l.file(settledFile))

	if l.exists(finalFile) {
		// The attempts are used up. Settled (the startup probe passes) but never ok: the device stays not ready, and since the
		// process does not end, the container is not restarted in a loop. The reason is in the termination message of the
		// run that gave up.
		_ = l.touch(settledFile)
		l.sys.Pause()
		return 0, true
	}
	if l.exists(startedFile) {
		// The service ran in this pod and ended; the kubelet restarted the container on a file system that is not clean and
		// without the list. The pod has to be recreated, which runs the init containers again.
		Message{Kind: KindRecycle, Reason: "the service ended; the pod is recreated to bootstrap a clean file system"}.write(l.cfg.TerminationLog)
		return ExitRecycle, true
	}

	attempt := l.cfg.Attempt
	if attempt <= 0 {
		attempt = l.nextAttempt()
	}
	final := attempt >= labchecks.BootstrapMaxAttempts
	if attempt > 1 {
		l.sys.Sleep(labchecks.BootstrapPause(attempt - 1))
	}

	if reason := l.attempt(); reason != "" {
		if final {
			_ = l.touch(finalFile)
		}
		Message{Kind: KindFailed, Attempt: attempt, Final: final, Reason: reason}.write(l.cfg.TerminationLog)
		return ExitFailed, true
	}
	return 0, false
}

// nextAttempt counts the attempts of a container that is restarted in place: the number is kept in the directory.
func (l *run) nextAttempt() int {
	n := 0
	if b, err := os.ReadFile(l.file(attemptsFile)); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	}
	n++
	_ = os.WriteFile(l.file(attemptsFile), []byte(fmt.Sprint(n)), 0o600)
	return n
}

// attempt does the work and returns the reason of a failure ("" on success). On success the list is erased and the
// markers are written, so the service starts with nothing of the bootstrap left in the container.
func (l *run) attempt() string {
	raw, err := os.ReadFile(l.file(listFile))
	if err != nil {
		return "the bootstrap list is missing"
	}
	var list labchecks.Bootstrap
	err = json.Unmarshal(raw, &list)
	clear(raw)
	if err != nil {
		return "the bootstrap list is not valid"
	}
	limit := time.Duration(list.TimeLimitSeconds) * time.Second
	if limit <= 0 {
		limit = labchecks.DefaultBootstrapTimeLimit * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	if err := placeAll(l.cfg.Root, l.acc, l.sys, list.Files); err != nil {
		return "placing " + err.Error()
	}
	if list.Script != "" {
		if reason := l.runScript(ctx, &list); reason != "" {
			return reason
		}
	}
	for i := range list.Files {
		clear(list.Files[i].Content)
	}
	list.Script = ""
	if err := l.erase(); err != nil {
		return "cannot erase the bootstrap list"
	}
	if err := l.touch(startedFile); err != nil {
		return "cannot write the markers"
	}
	_ = os.Remove(l.file(attemptsFile))
	if l.touch(okFile) != nil || l.touch(settledFile) != nil {
		return "cannot write the markers"
	}
	return ""
}

func (l *run) runScript(ctx context.Context, list *labchecks.Bootstrap) string {
	interpreter := list.Interpreter
	if interpreter == "" {
		interpreter = labchecks.DefaultInterpreter
	}
	err := l.sys.RunScript(ctx, interpreter, list.Script, l.sys.Environ())
	var exit *ScriptExitError
	var start *StartError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrTimeout):
		return fmt.Sprintf("the script did not finish in %d s", list.TimeLimitSeconds)
	case errors.As(err, &exit):
		return fmt.Sprintf("the script exited with code %d", exit.Code)
	case errors.As(err, &start):
		return "the script interpreter cannot be started"
	default:
		return "the script failed"
	}
}

// erase overwrites the list with zeros and removes it.
func (l *run) erase() error {
	p := l.file(listFile)
	if st, err := os.Stat(p); err == nil {
		if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write(make([]byte, st.Size()))
			_ = f.Close()
		}
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// startService becomes the service.
func (l *run) startService() int {
	fail := func(reason string) int {
		Message{Kind: KindStart, Reason: reason}.write(l.cfg.TerminationLog)
		return ExitStart
	}
	if len(l.cfg.Command) == 0 {
		return fail("no service command")
	}
	svcEnv := l.sys.Environ()
	var id *Identity
	{
		resolved, err := l.acc.Resolve(l.cfg.User)
		if err != nil {
			return fail(err.Error())
		}
		id = &resolved
		svcEnv = homeFor(svcEnv, resolved)
	}
	if l.cfg.WorkDir != "" {
		if err := l.prepareWorkDir(id); err != nil {
			return fail("cannot enter the working directory")
		}
	}
	path, err := lookPath(l.cfg.Command[0], svcEnv)
	if err != nil {
		return fail("the service command was not found")
	}
	if id != nil && (id.UID != 0 || id.GID != 0 || len(id.Groups) > 0) {
		if err := l.sys.Drop(*id); err != nil {
			return fail("cannot switch to the service user")
		}
	}
	if err := l.sys.Exec(path, l.cfg.Command, svcEnv); err != nil {
		return fail("the service command cannot be executed")
	}
	return 0 // only a fake System returns
}

func (l *run) prepareWorkDir(id *Identity) error {
	dir := filepath.Join(l.cfg.Root, l.cfg.WorkDir)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if id != nil {
			if err := l.sys.Chown(dir, id.UID, id.GID); err != nil {
				return err
			}
		}
	}
	return os.Chdir(dir)
}

// homeFor sets HOME the way a runtime does for the service user. The runtime already set HOME for root when the container
// started (the launcher runs as root), so a HOME that is root's own is replaced; a HOME the image or the device
// settings chose is kept.
func homeFor(vars []string, id Identity) []string {
	if id.UID == 0 {
		return vars
	}
	home := id.Home
	if home == "" {
		home = "/"
	}
	out := make([]string, 0, len(vars)+1)
	set := false
	for _, kv := range vars {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			set = true
			if v == "/root" || v == "" {
				kv = "HOME=" + home
			}
		}
		out = append(out, kv)
	}
	if !set {
		out = append(out, "HOME="+home)
	}
	return out
}

// lookPath finds the program like a shell does, with the PATH of the service.
func lookPath(prog string, vars []string) (string, error) {
	if strings.Contains(prog, "/") {
		return prog, nil
	}
	path := defaultPath
	for _, kv := range vars {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok && v != "" {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, prog)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}
