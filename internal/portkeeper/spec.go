// Package portkeeper is the in-device keeper of the console and serial ports of a device (docs/internal-docs/laboratory/specs/
// console-ports-design.md, 6.2). The launcher starts it right before it becomes the service; it holds one PTY per port for as long as
// the container lives, runs the console programs on their lines (respawned like a getty), links the serial ports into /dev, and serves
// each line on a unix socket in SocketDir for the device access service (probe port-attach), one client at a time.
//
// This file is the contract between the operator, which writes the port list of a device, and the keeper, which reads it. It imports
// nothing of the Kubernetes API, so the static in-device program stays small.
package portkeeper

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	// SpecDir is where the device container sees the port list (a downward API volume of the pod annotation names.AnnotationPortsSpec);
	// SpecFile is the file in it.
	SpecDir  = "/.cib/portspec"
	SpecFile = "ports.json"
	SpecPath = SpecDir + "/" + SpecFile
	// SocketDir is the memory volume with one unix socket per port (<name>.sock), directory 0700 root: only root of the device (the
	// device access service runs its attach as root) reaches it.
	SocketDir = "/.cib/ports"

	// NativeStdio and NativeTCP are the native console kinds the keeper bridges (vm-serial is never in a keeper's list).
	NativeStdio = "stdio"
	NativeTCP   = "tcp"

	// MaxConsolePorts and MaxSerialPorts repeat names.MaxConsolePorts and names.MaxSerialPorts (a test keeps them equal).
	MaxConsolePorts = 4
	MaxSerialPorts  = 16
	// MaxCommandItems and MaxCommandItem bound a console command (the rules of the terminal command).
	MaxCommandItems = 32
	MaxCommandItem  = 4096
	// DefaultCommand runs on a console port that names no command and no native source.
	DefaultCommand = "/bin/login"
)

var (
	consoleNameRE = regexp.MustCompile(`^(console|aux)(0|[1-9][0-9]?)$`)
	serialNameRE  = regexp.MustCompile(`^serial(0|[1-9][0-9]?)$`)
	serialPathRE  = regexp.MustCompile(`^/dev/tty[A-Z][A-Za-z0-9]{0,14}$`)
	// userRE is the user pattern of the terminal (deviceaccess.CheckSettings): a name or a uid, optionally with a group.
	userRE = regexp.MustCompile(`^([a-z_][a-z0-9_-]{0,31}\$?|[0-9]{1,10})(:([a-z_][a-z0-9_-]{0,31}\$?|[0-9]{1,10}))?$`)
)

// Spec is the port list of one device.
type Spec struct {
	Console []ConsolePort `json:"console,omitempty"`
	Serial  []SerialPort  `json:"serial,omitempty"`
}

// ConsolePort is a management line: the command runs on it, or Native names where the device already puts its console.
type ConsolePort struct {
	Name    string   `json:"name"`
	Command []string `json:"command,omitempty"`
	User    string   `json:"user,omitempty"`
	// Native is "", stdio or tcp; Port is the loopback port of tcp.
	Native string `json:"native,omitempty"`
	Port   int    `json:"port,omitempty"`
}

// SerialPort is a terminal line: Path is the character device inside the device.
type SerialPort struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// EffectiveCommand is the command of a manufactured console line: the declared one, else /bin/login.
func (c ConsolePort) EffectiveCommand() []string {
	if len(c.Command) == 0 {
		return []string{DefaultCommand}
	}
	return append([]string(nil), c.Command...)
}

// EffectiveUser is the user of the console command: the declared one, else root.
func (c ConsolePort) EffectiveUser() string {
	if c.User == "" {
		return "root"
	}
	return c.User
}

// Empty says the device has no port.
func (s Spec) Empty() bool { return len(s.Console) == 0 && len(s.Serial) == 0 }

// Stdio is the console port whose line is the service's own terminal, or nil.
func (s Spec) Stdio() *ConsolePort {
	for i := range s.Console {
		if s.Console[i].Native == NativeStdio {
			return &s.Console[i]
		}
	}
	return nil
}

// Names are the port names of the list, console ports first.
func (s Spec) Names() []string {
	out := make([]string, 0, len(s.Console)+len(s.Serial))
	for _, c := range s.Console {
		out = append(out, c.Name)
	}
	for _, p := range s.Serial {
		out = append(out, p.Name)
	}
	return out
}

// ErrInvalid wraps every refusal of Validate.
var ErrInvalid = errors.New("invalid port list")

// ValidPortName says the name is a console port or a serial port name.
func ValidPortName(name string) bool {
	return consoleNameRE.MatchString(name) || serialNameRE.MatchString(name)
}

// Validate checks the list the way the keeper needs it: names, limits, one stdio line, commands and users, unique serial paths. The
// operator and the agent check the same through package consolespec; the keeper checks again, since it runs as root.
func (s Spec) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	if len(s.Console) > MaxConsolePorts {
		return bad("more than %d console ports", MaxConsolePorts)
	}
	if len(s.Serial) > MaxSerialPorts {
		return bad("more than %d serial ports", MaxSerialPorts)
	}
	seen := map[string]bool{}
	stdio := 0
	for _, c := range s.Console {
		if !consoleNameRE.MatchString(c.Name) {
			return bad("console port name %q", c.Name)
		}
		if seen[c.Name] {
			return bad("port %q twice", c.Name)
		}
		seen[c.Name] = true
		switch c.Native {
		case "":
			if err := CheckCommand(c.Command, c.User); err != nil {
				return bad("console port %q: %v", c.Name, err)
			}
			if c.Port != 0 {
				return bad("console port %q: only a tcp console has a port", c.Name)
			}
		case NativeStdio:
			stdio++
			if len(c.Command) > 0 || c.User != "" || c.Port != 0 {
				return bad("console port %q: a stdio console takes no command, user or port", c.Name)
			}
		case NativeTCP:
			if c.Port < 1 || c.Port > 65535 || len(c.Command) > 0 || c.User != "" {
				return bad("console port %q: a tcp console takes a port from 1 to 65535 and no command or user", c.Name)
			}
		default:
			return bad("console port %q: unknown native kind %q", c.Name, c.Native)
		}
	}
	if stdio > 1 {
		return bad("more than one stdio console")
	}
	paths := map[string]bool{}
	for _, p := range s.Serial {
		if !serialNameRE.MatchString(p.Name) {
			return bad("serial port name %q", p.Name)
		}
		if seen[p.Name] {
			return bad("port %q twice", p.Name)
		}
		seen[p.Name] = true
		if !serialPathRE.MatchString(p.Path) {
			return bad("serial port %q: path %q", p.Name, p.Path)
		}
		if paths[p.Path] {
			return bad("serial path %q twice", p.Path)
		}
		paths[p.Path] = true
	}
	return nil
}

// CheckCommand applies the rules of the terminal command to a console command and user: at most 32 items of at most 4096 bytes, no
// NUL, the program an absolute path outside /proc and /sys (empty means the default), the user a name or a uid.
func CheckCommand(command []string, user string) error {
	if len(command) > MaxCommandItems {
		return fmt.Errorf("more than %d command items", MaxCommandItems)
	}
	for _, c := range command {
		if len(c) > MaxCommandItem || strings.ContainsRune(c, 0) {
			return errors.New("a command item is too long or contains NUL")
		}
	}
	if len(command) > 0 {
		prog := path.Clean(command[0])
		if !strings.HasPrefix(command[0], "/") || prog == "/proc" || prog == "/sys" || strings.HasPrefix(prog, "/proc/") || strings.HasPrefix(prog, "/sys/") {
			return errors.New("the command must be an absolute path outside /proc and /sys")
		}
	}
	if user != "" && !userRE.MatchString(user) {
		return fmt.Errorf("bad user %q", user)
	}
	return nil
}

// Parse reads and validates a port list.
func Parse(b []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return s, s.Validate()
}

// SocketPath is the unix socket of a port.
func SocketPath(dir, port string) string { return path.Join(dir, port+".sock") }
