// Package consolespec holds the rules of console ports, serial ports and console cables that the CRD cannot express
// (docs/internal-docs/laboratory/specs/console-ports-design.md, 4.3 and 14.1), and the port list the operator gives the in-device port
// keeper. The management agent refuses a spec that breaks them (InvalidArgument, ErrorInfo reason CONSOLE_CABLE_INVALID) and the
// operator fails a Lab that slipped through (condition reason ConsoleCableInvalid): both call Validate.
package consolespec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/portkeeper"
)

// ReasonInvalid is the reason of the Ready condition of a Lab whose ports or cables break a rule (the agent's reason is
// client.ReasonConsoleCableInvalid).
const ReasonInvalid = "ConsoleCableInvalid"

// Error is a refusal: the device and the port it is about (either may be empty) and why.
type Error struct {
	Device, Port string
	Text         string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(ReasonInvalid + ": ")
	if e.Device != "" {
		fmt.Fprintf(&b, "device %q: ", e.Device)
	}
	if e.Port != "" {
		fmt.Fprintf(&b, "the port %q ", e.Port)
	}
	b.WriteString(e.Text)
	return b.String()
}

// Reason is the coded reason of the refusal (the operator's condition reason).
func (e *Error) Reason() string { return ReasonInvalid }

func refuse(device, port, format string, a ...any) error {
	return &Error{Device: device, Port: port, Text: fmt.Sprintf(format, a...)}
}

// Validate checks the ports of every device and the console cables of the lab. nil when the lab has none.
//
//nolint:gocyclo // one pass over the rules of 4.3; splitting it would scatter them
func Validate(spec *laboratoryv1alpha1.LabSpec) error {
	type port struct{ serial bool }
	ports := map[string]map[string]port{} // device -> port -> kind
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if !d.HasPorts() {
			continue
		}
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			return refuse(d.Name, "", "has console or serial ports: only a container device has them")
		}
		if s := d.Service; s == nil || len(s.Command)+len(s.Args) == 0 || s.User == "" {
			return refuse(d.Name, "", "has console or serial ports, so it starts through the launcher and needs a service command and its user (service.command, service.user)")
		}
		ifaces := map[string]bool{}
		for _, nic := range d.Interfaces {
			ifaces[nic.Name] = true
		}
		mine := map[string]port{}
		for _, c := range d.ConsolePorts {
			if ifaces[c.Name] {
				return refuse(d.Name, c.Name, "has the name of an interface of the device: ports and interfaces share one name space")
			}
			if c.Native != nil && c.Native.Kind == laboratoryv1alpha1.NativeConsoleVMSerial && d.Profile.Effective() != laboratoryv1alpha1.DeviceProfileVM {
				return refuse(d.Name, c.Name, "is vm-serial, which the platform sets for console0 of a vm-profile device only")
			}
			mine[c.Name] = port{}
		}
		for _, s := range d.SerialPorts {
			if ifaces[s.Name] {
				return refuse(d.Name, s.Name, "has the name of an interface of the device: ports and interfaces share one name space")
			}
			mine[s.Name] = port{serial: true}
		}
		if err := KeeperSpec(deviceSpecOf(d)).Validate(); err != nil {
			return refuse(d.Name, "", "%s", strings.TrimPrefix(err.Error(), portkeeper.ErrInvalid.Error()+": "))
		}
		ports[d.Name] = mine
	}
	if len(spec.ConsoleCables) > names.MaxConsoleCablesPerLab {
		return refuse("", "", "the lab has more than %d console cables", names.MaxConsoleCablesPerLab)
	}
	used := map[string]bool{}
	for _, c := range spec.ConsoleCables {
		for _, end := range []struct {
			ref    laboratoryv1alpha1.PortRef
			serial bool
		}{{c.Serial, true}, {c.Console, false}} {
			p, ok := ports[end.ref.Device][end.ref.Port]
			switch {
			case !ok && end.serial:
				return refuse(end.ref.Device, end.ref.Port, "is unresolved: the serial end of a console cable names a serial port the device declares")
			case !ok:
				return refuse(end.ref.Device, end.ref.Port, "is unresolved: the console end of a console cable names a console port the device declares")
			case p.serial != end.serial && end.serial:
				return refuse(end.ref.Device, end.ref.Port, "is not a serial port: the serial end of a console cable is a serial port")
			case p.serial != end.serial:
				return refuse(end.ref.Device, end.ref.Port, "is not a console port: the console end of a console cable is a console port")
			}
		}
		if c.Serial.Device == c.Console.Device {
			return refuse(c.Serial.Device, "", "a console cable joins two different devices, not the device to itself")
		}
		for _, ref := range []laboratoryv1alpha1.PortRef{c.Serial, c.Console} {
			key := ref.Device + "/" + ref.Port
			if used[key] {
				return refuse(ref.Device, ref.Port, "is used by more than one console cable")
			}
			used[key] = true
		}
	}
	return nil
}

func deviceSpecOf(d *laboratoryv1alpha1.DeviceTemplate) *laboratoryv1alpha1.DeviceSpec {
	return &laboratoryv1alpha1.DeviceSpec{Type: d.Type, ConsolePorts: d.ConsolePorts, SerialPorts: d.SerialPorts}
}

// KeeperSpec is the port list the keeper of the device gets (serial paths resolved to /dev/ttyCON<n> when not declared). vm-serial
// ports are not the keeper's: the line owner opens the hypervisor's socket (6.4), so they are left out.
func KeeperSpec(d *laboratoryv1alpha1.DeviceSpec) portkeeper.Spec {
	var out portkeeper.Spec
	for _, c := range d.ConsolePorts {
		p := portkeeper.ConsolePort{Name: c.Name, Command: append([]string(nil), c.Command...), User: c.User}
		if c.Native != nil {
			if c.Native.Kind == laboratoryv1alpha1.NativeConsoleVMSerial {
				continue
			}
			p.Native, p.Port = c.Native.Kind, int(c.Native.Port)
		}
		out.Console = append(out.Console, p)
	}
	for _, s := range d.SerialPorts {
		out.Serial = append(out.Serial, portkeeper.SerialPort{Name: s.Name, Path: s.DevicePath()})
	}
	return out
}

// Marshal is the JSON of a port list, the value of the pod annotation names.AnnotationPortsSpec and the file the keeper reads.
func Marshal(s portkeeper.Spec) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// PortsHash is the label value names.LabelPorts of a port list: the first 32 hex characters of the SHA-256 of its JSON. The device
// access service trusts sandbox labels only, so it checks a port name against the list whose digest this is, without the Kubernetes API.
func PortsHash(s portkeeper.Spec) string {
	sum := sha256.Sum256([]byte(Marshal(s)))
	return hex.EncodeToString(sum[:])[:32]
}
