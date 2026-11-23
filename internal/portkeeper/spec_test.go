package portkeeper

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/names"
)

func TestLimitsFollowNames(t *testing.T) {
	if MaxConsolePorts != names.MaxConsolePorts || MaxSerialPorts != names.MaxSerialPorts {
		t.Fatal("the keeper's limits must equal the names constants")
	}
}

func TestSpecValidate(t *testing.T) {
	ok := Spec{
		Console: []ConsolePort{{Name: "console0", Command: []string{"/usr/bin/vtysh"}}, {Name: "aux0"}, {Name: "console1", Native: NativeTCP, Port: 2001},
			{Name: "console2", Native: NativeStdio}},
		Serial: []SerialPort{{Name: "serial0", Path: "/dev/ttyCON0"}, {Name: "serial1", Path: "/dev/ttyUSB1"}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid list refused: %v", err)
	}
	for name, c := range map[string]struct {
		mod  func(*Spec)
		want string
	}{
		"bad console name":   {func(s *Spec) { s.Console[0].Name = "tty0" }, "console port name"},
		"bad serial name":    {func(s *Spec) { s.Serial[0].Name = "com1" }, "serial port name"},
		"duplicate":          {func(s *Spec) { s.Console[1].Name = "console0" }, "twice"},
		"relative command":   {func(s *Spec) { s.Console[0].Command = []string{"vtysh"} }, "absolute path"},
		"proc command":       {func(s *Spec) { s.Console[0].Command = []string{"/proc/../proc/self/exe"} }, "absolute path"},
		"bad user":           {func(s *Spec) { s.Console[0].User = "Root Admin" }, "bad user"},
		"two stdio":          {func(s *Spec) { s.Console[1] = ConsolePort{Name: "aux0", Native: NativeStdio} }, "more than one stdio"},
		"tcp port 0":         {func(s *Spec) { s.Console[2].Port = 0 }, "tcp console"},
		"stdio with command": {func(s *Spec) { s.Console[3].Command = []string{"/bin/sh"} }, "stdio console"},
		"unknown native":     {func(s *Spec) { s.Console[3].Native = "vm-serial" }, "unknown native"},
		"path /dev/console":  {func(s *Spec) { s.Serial[0].Path = "/dev/console" }, "path"},
		"path twice":         {func(s *Spec) { s.Serial[1].Path = "/dev/ttyCON0" }, "serial path"},
		"five console":       {func(s *Spec) { s.Console = append(s.Console, ConsolePort{Name: "aux1"}) }, "more than 4"},
	} {
		s := ok
		s.Console = append([]ConsolePort(nil), ok.Console...)
		s.Serial = append([]SerialPort(nil), ok.Serial...)
		c.mod(&s)
		err := s.Validate()
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestParseRefusesUnknownKeysAndDefaults(t *testing.T) {
	if _, err := Parse([]byte(`{"console":[{"name":"console0","shell":true}]}`)); err == nil {
		t.Fatal("an unknown key must be refused")
	}
	s, err := Parse([]byte(`{"console":[{"name":"console0"}],"serial":[{"name":"serial0","path":"/dev/ttyCON0"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Console[0]; c.EffectiveCommand()[0] != "/bin/login" || c.EffectiveUser() != "root" {
		t.Fatalf("defaults: %+v", c)
	}
	if got := strings.Join(s.Names(), ","); got != "console0,serial0" {
		t.Fatalf("names: %s", got)
	}
	if SocketPath(SocketDir, "console0") != "/.cib/ports/console0.sock" {
		t.Fatal("socket path")
	}
}

// Hash is the digest the operator stamps on the pod (names.LabelPorts): stable for the same list, different for another one, and the
// lookups name a port only with the kind it has.
func TestHashAndLookups(t *testing.T) {
	s := Spec{Console: []ConsolePort{{Name: "console0"}}, Serial: []SerialPort{{Name: "serial0", Path: "/dev/ttyCON0"}}}
	h := Hash(s)
	if len(h) != 32 || Hash(s) != h {
		t.Fatalf("hash %q", h)
	}
	other := s
	other.Serial = []SerialPort{{Name: "serial1", Path: "/dev/ttyCON1"}}
	if Hash(other) == h {
		t.Fatal("another list must have another hash")
	}
	if !s.HasConsole("console0") || s.HasConsole("serial0") || !s.HasSerial("serial0") || s.HasSerial("console0") {
		t.Fatal("lookups by kind")
	}
}

func TestKernelLogInTheSpec(t *testing.T) {
	ok := Spec{Console: []ConsolePort{{Name: "console0", KernelLog: true}, {Name: "aux0"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := Parse([]byte(`{"console":[{"name":"console0","kernel_log":true}]}`))
	if err != nil || !s.Console[0].KernelLog {
		t.Fatalf("parse: %+v %v", s, err)
	}
	for name, bad := range map[string]Spec{
		"two":   {Console: []ConsolePort{{Name: "console0", KernelLog: true}, {Name: "aux0", KernelLog: true}}},
		"stdio": {Console: []ConsolePort{{Name: "console0", Native: NativeStdio, KernelLog: true}}},
		"tcp":   {Console: []ConsolePort{{Name: "console0", Native: NativeTCP, Port: 23, KernelLog: true}}},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The digest leaves the kernel log out: an older release (whose list has no kernel_log) computes the same digest for the same ports, and
// the list without the flag is what an older parser takes.
func TestHashIgnoresTheKernelLog(t *testing.T) {
	with := Spec{Console: []ConsolePort{{Name: "console0", KernelLog: true}, {Name: "aux0"}}, Serial: []SerialPort{{Name: "serial0", Path: "/dev/ttyCON0"}}}
	without := with.WithoutKernelLog()
	if Hash(with) != Hash(without) || !with.Console[0].KernelLog || without.Console[0].KernelLog {
		t.Fatal("the kernel log must not change the digest, and WithoutKernelLog must copy")
	}
	sum := sha256.Sum256([]byte(`{"console":[{"name":"console0"},{"name":"aux0"}],"serial":[{"name":"serial0","path":"/dev/ttyCON0"}]}`))
	if Hash(with) != hex.EncodeToString(sum[:])[:32] {
		t.Fatal("the digest differs from the one of a release before the kernel log")
	}
	if strings.Contains(string(Marshal(without)), "kernel_log") {
		t.Fatal("kernel_log left in the stripped list")
	}
}
