package deviceaccess

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// ExecUser is a user of the device, resolved inside it (see the probe's resolve-user): the runtime is given numbers only.
type ExecUser struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups,omitempty"`
	Home   string   `json:"home,omitempty"`
}

// ExecArgs is everything the service chooses for an exec. Everything else comes from the container.
type ExecArgs struct {
	Args     []string
	Terminal bool
	// Cols and Rows are the initial terminal size (Terminal only).
	Cols, Rows uint16
	// User, when set, replaces the user of the container's process.
	User *ExecUser
}

const (
	defaultCommand = "/bin/sh"
	// termType is what the terminal announces; the browser's xterm understands it.
	termType = "xterm-256color"
	// MaxCommandItems and MaxCommandItem bound the terminal command the lab author set.
	MaxCommandItems = 32
	MaxCommandItem  = 4096
)

var userRE = regexp.MustCompile(`^([a-z_][a-z0-9_-]{0,31}\$?|[0-9]{1,10})(:([a-z_][a-z0-9_-]{0,31}\$?|[0-9]{1,10}))?$`)

// ErrBadSettings is returned for a command or user the service will not run.
var ErrBadSettings = errors.New("invalid terminal settings")

// CheckSettings validates the terminal command and user and returns the command to run (the default shell when empty).
func CheckSettings(command []string, user string) ([]string, error) {
	if len(command) == 0 {
		command = []string{defaultCommand}
	}
	if len(command) > MaxCommandItems {
		return nil, fmt.Errorf("%w: too many command items", ErrBadSettings)
	}
	for _, c := range command {
		if len(c) > MaxCommandItem || strings.ContainsRune(c, 0) {
			return nil, fmt.Errorf("%w: bad command item", ErrBadSettings)
		}
	}
	// The program must be an absolute path outside /proc and /sys: a path under them is a way to run something that is not a program
	// of the device, and a relative name would depend on the PATH of the container.
	if prog := command[0]; !strings.HasPrefix(prog, "/") || underVirtualFS(path.Clean(prog)) {
		return nil, fmt.Errorf("%w: the command must be an absolute path outside /proc and /sys", ErrBadSettings)
	}
	if user != "" && !userRE.MatchString(user) {
		return nil, fmt.Errorf("%w: bad user", ErrBadSettings)
	}
	return command, nil
}

// ProcessFor builds the process an exec runs, the way the CRI plugin does for `kubectl exec`: a copy of the process block of the
// container (user, capabilities, no_new_privs, seccomp and the rest of the container's restrictions, the environment base, working
// directory, rlimits), then only the arguments, the terminal, TERM and, when asked, the user are set. The capabilities are never
// built from scratch, because the runtime does not clamp them to the container's: a set written here would be exactly what the
// process gets. Everything else of the container's process block (no_new_privs, the AppArmor profile, the SELinux label, rlimits, the
// OOM score adjustment) is carried over by the copy.
func ProcessFor(c *specs.Process, a ExecArgs) (*specs.Process, error) {
	if c == nil {
		return nil, errors.New("the container has no process spec")
	}
	if len(a.Args) == 0 {
		return nil, errors.New("no command")
	}
	// A deep copy: the spec of the container may be cached by the runtime client and must stay as it is.
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var p specs.Process
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p.Args = append([]string(nil), a.Args...)
	p.CommandLine = ""
	p.Terminal = a.Terminal
	p.ConsoleSize = nil
	if a.Terminal {
		p.Env = setVar(p.Env, "TERM", termType)
		if a.Cols > 0 && a.Rows > 0 {
			p.ConsoleSize = &specs.Box{Width: uint(a.Cols), Height: uint(a.Rows)}
		}
	}
	if p.Cwd == "" {
		p.Cwd = "/"
	}
	if u := a.User; u != nil {
		p.User = specs.User{UID: u.UID, GID: u.GID, AdditionalGids: append([]uint32(nil), u.Groups...)}
		if u.Home != "" {
			p.Env = setVar(p.Env, "HOME", u.Home)
		}
		// What runc, the CRI plugin and Docker do for a user that is not root: the container's capability sets are root's, and an
		// ordinary user keeps none of them (only the bounding set stays, as the limit of what a setuid program could regain). Without
		// this a console opened as an ordinary user would be a shell with root's effective capabilities. A root exec keeps the sets of
		// the container, and the kernel never gives it more than the bounding set.
		if u.UID != 0 && p.Capabilities != nil {
			p.Capabilities.Effective, p.Capabilities.Permitted, p.Capabilities.Ambient, p.Capabilities.Inheritable = nil, nil, nil, nil
		}
	}
	return &p, nil
}

func underVirtualFS(clean string) bool {
	for _, d := range []string{"/proc", "/sys"} {
		if clean == d || strings.HasPrefix(clean, d+"/") {
			return true
		}
	}
	return false
}

// CommandHash is a short fingerprint of a command line for the session log (see wire.CommandHash).
func CommandHash(command []string) string { return wire.CommandHash(command) }

func setVar(vars []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(vars)+1)
	for _, e := range vars {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+value)
}
