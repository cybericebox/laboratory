package consolespec

import (
	"errors"
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/portkeeper"
)

func svc() *laboratoryv1alpha1.DeviceService {
	return &laboratoryv1alpha1.DeviceService{Command: []string{"/usr/lib/frr/docker-start"}, User: "root"}
}

func goodLab() *laboratoryv1alpha1.LabSpec {
	return &laboratoryv1alpha1.LabSpec{
		Devices: []laboratoryv1alpha1.DeviceTemplate{
			{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer, Service: svc(),
				Interfaces:   []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}},
				ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0", Command: []string{"/usr/bin/vtysh"}}, {Name: "aux0"}}},
			{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeContainer, Service: svc(),
				ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0", Native: &laboratoryv1alpha1.NativeConsoleSpec{Kind: "stdio"}}}},
			{Name: "pc1", Type: laboratoryv1alpha1.DeviceTypeContainer, Service: svc(),
				SerialPorts: []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}, {Name: "serial1"}, {Name: "serial2"}}},
			{Name: "plain", Type: laboratoryv1alpha1.DeviceTypeContainer},
		},
		ConsoleCables: []laboratoryv1alpha1.ConsoleCableTemplate{
			cable("pc1", "serial0", "r1", "console0"),
			cable("pc1", "serial1", "sw", "console0"),
		},
	}
}

func cable(sd, sp, cd, cp string) laboratoryv1alpha1.ConsoleCableTemplate {
	return laboratoryv1alpha1.ConsoleCableTemplate{Serial: laboratoryv1alpha1.PortRef{Device: sd, Port: sp}, Console: laboratoryv1alpha1.PortRef{Device: cd, Port: cp}}
}

func TestValidAndUnpluggedPortsPass(t *testing.T) {
	if err := Validate(goodLab()); err != nil {
		t.Fatalf("valid lab refused: %v", err)
	}
	l := goodLab()
	l.ConsoleCables = nil // every port unplugged
	if err := Validate(l); err != nil {
		t.Fatalf("unplugged ports are valid: %v", err)
	}
	if err := Validate(&laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "a"}}}); err != nil {
		t.Fatalf("a lab without ports: %v", err)
	}
}

// Every rule the CRD cannot express (and the ones it can, for a spec that reaches the agent as JSON) is refused with the device and the
// port named and the reason ConsoleCableInvalid.
func TestValidateRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		mod  func(*laboratoryv1alpha1.LabSpec)
		want string
	}{
		"port named like a NIC": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[0].Interfaces = append(l.Devices[0].Interfaces, laboratoryv1alpha1.InterfaceSpec{Name: "aux0"})
		}, `device "r1": the port "aux0" has the name of an interface`},
		"two stdio": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[1].ConsolePorts = append(l.Devices[1].ConsolePorts, laboratoryv1alpha1.ConsolePortSpec{Name: "aux0", Native: &laboratoryv1alpha1.NativeConsoleSpec{Kind: "stdio"}})
		}, "more than one stdio"},
		"vm-serial": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[1].ConsolePorts[0].Native.Kind = "vm-serial"
		}, "vm-serial"},
		"ports on a switch": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[3].Type = laboratoryv1alpha1.DeviceTypeHub
			l.Devices[3].SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
		}, "only a container device"},
		"no service":      {func(l *laboratoryv1alpha1.LabSpec) { l.Devices[0].Service = nil }, "needs a service command"},
		"no service user": {func(l *laboratoryv1alpha1.LabSpec) { l.Devices[0].Service.User = "" }, "needs a service command"},
		"serial path twice": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[2].SerialPorts[0].Path = "/dev/ttyCON1"
		}, "serial path"},
		"relative command":      {func(l *laboratoryv1alpha1.LabSpec) { l.Devices[0].ConsolePorts[0].Command = []string{"vtysh"} }, "absolute path"},
		"unknown serial device": {func(l *laboratoryv1alpha1.LabSpec) { l.ConsoleCables[0].Serial.Device = "ghost" }, "unresolved"},
		"unknown console port":  {func(l *laboratoryv1alpha1.LabSpec) { l.ConsoleCables[0].Console.Port = "console7" }, "unresolved"},
		"serial end on a console port": {func(l *laboratoryv1alpha1.LabSpec) {
			l.ConsoleCables[0].Serial = laboratoryv1alpha1.PortRef{Device: "r1", Port: "aux0"}
		}, "serial port"},
		"two serial ends": {func(l *laboratoryv1alpha1.LabSpec) {
			l.ConsoleCables[0].Console = laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial2"}
		}, "console port"},
		"NIC as an end": {func(l *laboratoryv1alpha1.LabSpec) {
			l.ConsoleCables[0].Console = laboratoryv1alpha1.PortRef{Device: "r1", Port: "eth0"}
		}, "console port"},
		"same device": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[2].ConsolePorts = []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}
			l.ConsoleCables[0].Console = laboratoryv1alpha1.PortRef{Device: "pc1", Port: "console0"}
		}, "two different devices"},
		"console port twice": {func(l *laboratoryv1alpha1.LabSpec) {
			l.ConsoleCables = append(l.ConsoleCables, cable("pc1", "serial2", "r1", "console0"))
		}, "used by more than one console cable"},
		"serial port twice": {func(l *laboratoryv1alpha1.LabSpec) {
			l.ConsoleCables = append(l.ConsoleCables, cable("pc1", "serial0", "r1", "aux0"))
		}, "used by more than one console cable"},
		"port list too big for its annotation": {func(l *laboratoryv1alpha1.LabSpec) {
			long := make([]string, 32)
			long[0] = "/bin/sh"
			for i := 1; i < len(long); i++ {
				long[i] = strings.Repeat("x", 4000)
			}
			l.Devices[0].ConsolePorts[0].Command = long
		}, "port list"},
		"kernel log on a container device": {func(l *laboratoryv1alpha1.LabSpec) { l.Devices[0].ConsolePorts[0].KernelLog = true },
			`device "r1": the port "console0" has kernelLog, which only a microvm device has`},
		"kernel log on an explicit container profile": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[0].Profile = laboratoryv1alpha1.DeviceProfileContainer
			l.Devices[0].ConsolePorts[0].KernelLog = true
		}, "only a microvm device"},
		"kernel log on two ports": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[0].Profile = laboratoryv1alpha1.DeviceProfileMicroVM
			l.Devices[0].ConsolePorts[0].KernelLog = true
			l.Devices[0].ConsolePorts[1].KernelLog = true
		}, "more than one console port with the kernel log"},
		"kernel log on a native console": {func(l *laboratoryv1alpha1.LabSpec) {
			l.Devices[1].Profile = laboratoryv1alpha1.DeviceProfileMicroVM
			l.Devices[1].ConsolePorts[0].KernelLog = true
		}, "kernel log"},
		"too many cables": {func(l *laboratoryv1alpha1.LabSpec) {
			for len(l.ConsoleCables) <= 64 {
				l.ConsoleCables = append(l.ConsoleCables, cable("pc1", "serial0", "r1", "console0"))
			}
		}, "more than 64"},
	} {
		l := goodLab()
		c.mod(l)
		err := Validate(l)
		var ce *Error
		if !errors.As(err, &ce) || ce.Reason() != ReasonInvalid || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestKeeperSpecAndDigest(t *testing.T) {
	l := goodLab()
	d := &laboratoryv1alpha1.DeviceSpec{Type: laboratoryv1alpha1.DeviceTypeContainer, ConsolePorts: l.Devices[0].ConsolePorts,
		SerialPorts: []laboratoryv1alpha1.SerialPortSpec{{Name: "serial3"}, {Name: "serial4", Path: "/dev/ttyS4"}},
	}
	d.ConsolePorts = append(d.ConsolePorts, laboratoryv1alpha1.ConsolePortSpec{Name: "console1", Native: &laboratoryv1alpha1.NativeConsoleSpec{Kind: "tcp", Port: 2001}})
	ks := KeeperSpec(d)
	want := portkeeper.Spec{
		Console: []portkeeper.ConsolePort{{Name: "console0", Command: []string{"/usr/bin/vtysh"}}, {Name: "aux0"}, {Name: "console1", Native: "tcp", Port: 2001}},
		Serial:  []portkeeper.SerialPort{{Name: "serial3", Path: "/dev/ttyCON3"}, {Name: "serial4", Path: "/dev/ttyS4"}},
	}
	if PortsHash(ks) != PortsHash(want) || len(ks.Console) != 3 || ks.Serial[0].Path != "/dev/ttyCON3" || ks.Console[2].Port != 2001 {
		t.Fatalf("keeper spec %+v", ks)
	}
	if err := ks.Validate(); err != nil {
		t.Fatalf("the keeper must accept what the operator writes: %v", err)
	}
	h := PortsHash(ks)
	if len(h) != 32 {
		t.Fatalf("digest %q", h)
	}
	ks.Console[0].Command = []string{"/bin/sh"}
	if PortsHash(ks) == h {
		t.Fatal("a changed command must change the digest")
	}
	if !KeeperSpec(&laboratoryv1alpha1.DeviceSpec{}).Empty() {
		t.Fatal("no ports, empty list")
	}
}

// The kernel log is accepted on one manufactured console port of a microvm device, and reaches the keeper only from a microvm device:
// the operator is the gate (on a container device the kernel is the node's), so a Device that slipped through without the profile never
// gives the keeper the flag.
func TestKernelLogOnlyFromAMicroVM(t *testing.T) {
	l := goodLab()
	l.Devices[0].Profile = laboratoryv1alpha1.DeviceProfileMicroVM
	l.Devices[0].ConsolePorts[0].KernelLog = true
	if err := Validate(l); err != nil {
		t.Fatalf("kernel log on a microvm device refused: %v", err)
	}
	ks := KeeperSpec(&laboratoryv1alpha1.DeviceSpec{Type: laboratoryv1alpha1.DeviceTypeContainer, Profile: laboratoryv1alpha1.DeviceProfileMicroVM,
		ConsolePorts: l.Devices[0].ConsolePorts})
	if !ks.Console[0].KernelLog || ks.Console[1].KernelLog {
		t.Fatalf("microvm keeper spec %+v", ks)
	}
	if err := ks.Validate(); err != nil {
		t.Fatalf("the keeper must accept it: %v", err)
	}
	for _, p := range []laboratoryv1alpha1.DeviceProfile{"", laboratoryv1alpha1.DeviceProfileContainer, laboratoryv1alpha1.DeviceProfileVM} {
		ks := KeeperSpec(&laboratoryv1alpha1.DeviceSpec{Type: laboratoryv1alpha1.DeviceTypeContainer, Profile: p, ConsolePorts: l.Devices[0].ConsolePorts})
		if ks.Console[0].KernelLog {
			t.Fatalf("profile %q: the keeper must never get the kernel log", p)
		}
	}
	// a port without the flag keeps the digest it had before the field existed
	if Marshal(KeeperSpec(&laboratoryv1alpha1.DeviceSpec{ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}})) != `{"console":[{"name":"console0"}]}` {
		t.Fatal("kernel_log must be left out when off")
	}
}
