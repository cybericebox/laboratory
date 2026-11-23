package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Console ports, serial ports and console cables (docs/internal-docs/laboratory/specs/console-ports-design.md).
//
// A console port is a management line of a device (console0, aux0): a program runs on it, by default /bin/login. A serial port is a
// terminal line of a connector device (a PC, a laptop, a tools device): inside the device it is a character device, /dev/ttyCON<n>. A
// console cable joins exactly one serial port to exactly one console port of another device of the same lab. The bytes of a line never
// touch the lab network or a device NIC: the in-device port keeper holds the PTYs and the device access services of the nodes move the
// bytes between them.

// Native console kinds (NativeConsoleSpec.Kind).
const (
	// NativeConsoleStdio: the console is the service's own terminal (stdin, stdout and stderr are the line).
	NativeConsoleStdio = "stdio"
	// NativeConsoleTCP: the device puts its console on a TCP port of its own loopback (the "telnet to the console port" of emulators).
	NativeConsoleTCP = "tcp"
	// NativeConsoleVMSerial is set by the platform for console0 of a vm-profile device (the hypervisor's serial device); never by an
	// author. The vm profile is reserved, so it is not in the CRD enum and is refused today.
	NativeConsoleVMSerial = "vm-serial"
)

// DefaultConsoleCommand runs on a console port that names neither a command nor a native source.
var DefaultConsoleCommand = []string{"/bin/login"}

// ConsolePortSpec is a management line of a device. At most one of Command and Native; neither means the default command (/bin/login).
// +kubebuilder:validation:XValidation:rule="!has(self.native) || ((!has(self.command) || size(self.command) == 0) && !has(self.user))",message="a console port takes a command (with a user) or a native source, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.command) || size(self.command) == 0 || (self.command[0].startsWith('/') && !self.command[0].startsWith('/proc/') && !self.command[0].startsWith('/sys/'))",message="the console command starts with an absolute program path outside /proc and /sys"
// +kubebuilder:validation:XValidation:rule="!has(self.kernelLog) || !self.kernelLog || !has(self.native)",message="kernelLog is for a console port that runs a command, not for a native console"
type ConsolePortSpec struct {
	// Name is console<n> or aux<n>, n from 0 to 99. Port names and interface names share one space per device.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=9
	// +kubebuilder:validation:Pattern=`^(console|aux)(0|[1-9][0-9]?)$`
	Name string `json:"name"`
	// Command runs on the line, respawned when it ends; empty means /bin/login. The same rules as the terminal command: an absolute program
	// path outside /proc and /sys, at most 32 arguments of at most 4096 bytes.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=4096
	// +optional
	Command []string `json:"command,omitempty"`
	// User of the command; empty means root (login needs root).
	// +kubebuilder:validation:MaxLength=128
	// +optional
	User string `json:"user,omitempty"`
	// Native says where the device already puts its console (an emulator); the keeper bridges it to the line instead of running a command.
	// +optional
	Native *NativeConsoleSpec `json:"native,omitempty"`
	// KernelLog shows the guest kernel's messages on the line, like a kernel console: when a client attaches, the keeper first writes the
	// kernel's ring buffer (every message since boot), then follows new kernel messages, beside the program of the port (the login
	// prompt). Only on a microvm device (its own kernel; on a container device the kernel is the node's and is never shown), at most one
	// port of a device, not with native. Contract 3.2 (GetFeatures.console_cables.kernel_log).
	// +optional
	KernelLog bool `json:"kernelLog,omitempty"`
}

// NativeConsoleSpec is where the device already puts its console.
// +kubebuilder:validation:XValidation:rule="self.kind != 'tcp' || (has(self.port) && self.port > 0)",message="tcp needs a port"
// +kubebuilder:validation:XValidation:rule="self.kind == 'tcp' || !has(self.port)",message="only a tcp console has a port"
type NativeConsoleSpec struct {
	// Kind is stdio (the service's own terminal) or tcp (a port on the device's loopback). vm-serial (NativeConsoleVMSerial) is reserved
	// with the vm profile and not in the enum yet, like the profile itself.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=stdio;tcp
	Kind string `json:"kind"`
	// Port on 127.0.0.1 inside the device (tcp only).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`
}

// SerialPortSpec is a terminal line of a connector device.
type SerialPortSpec struct {
	// Name is serial<n>, n from 0 to 99.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=8
	// +kubebuilder:validation:Pattern=`^serial(0|[1-9][0-9]?)$`
	Name string `json:"name"`
	// Path of the character device inside the device; empty means /dev/ttyCON<n>, n the number of the name. /dev/tty, /dev/console and
	// /dev/ptmx are never a path (the pattern refuses them).
	// +kubebuilder:validation:MaxLength=24
	// +kubebuilder:validation:Pattern=`^/dev/tty[A-Z][A-Za-z0-9]{0,14}$`
	// +optional
	Path string `json:"path,omitempty"`
}

// DevicePath is the character device of the serial port inside the device: the declared path, else /dev/ttyCON<n>.
func (s SerialPortSpec) DevicePath() string {
	if s.Path != "" {
		return s.Path
	}
	return "/dev/ttyCON" + s.Name[len("serial"):]
}

// EffectiveCommand is the command of a manufactured console port: the declared one, else /bin/login. Nil for a native port.
func (c ConsolePortSpec) EffectiveCommand() []string {
	if c.Native != nil {
		return nil
	}
	if len(c.Command) == 0 {
		return append([]string(nil), DefaultConsoleCommand...)
	}
	return append([]string(nil), c.Command...)
}

// PortRef names one port of one device of the same Lab.
type PortRef struct {
	// Device is the name of a device of the lab (DeviceTemplate.name).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=35
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Device string `json:"device"`
	// Port is the name of a console port or a serial port of that device.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=9
	// +kubebuilder:validation:Pattern=`^(serial|console|aux)(0|[1-9][0-9]?)$`
	Port string `json:"port"`
}

// ConsoleCableTemplate is an entry of Lab.spec.consoleCables: one serial end, one console end, on two devices.
// +kubebuilder:validation:XValidation:rule="self.serial.port.startsWith('serial')",message="the serial end names a serial port"
// +kubebuilder:validation:XValidation:rule="!self.console.port.startsWith('serial')",message="the console end names a console port"
// +kubebuilder:validation:XValidation:rule="self.serial.device != self.console.device",message="the two ends of a console cable are on two different devices"
type ConsoleCableTemplate struct {
	// +kubebuilder:validation:Required
	Serial PortRef `json:"serial"`
	// +kubebuilder:validation:Required
	Console PortRef `json:"console"`
}

// ConsoleCableSpec is the desired state of a ConsoleCable.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a console cable is immutable"
// +kubebuilder:validation:XValidation:rule="self.serial.port.startsWith('serial')",message="the serial end names a serial port"
// +kubebuilder:validation:XValidation:rule="!self.console.port.startsWith('serial')",message="the console end names a console port"
// +kubebuilder:validation:XValidation:rule="self.serial.device != self.console.device",message="the two ends of a console cable are on two different devices"
type ConsoleCableSpec struct {
	// LabRef is the name of the parent Lab; both ends are devices of it.
	// +kubebuilder:validation:Required
	LabRef string `json:"labRef"`
	// +kubebuilder:validation:Required
	Serial PortRef `json:"serial"`
	// +kubebuilder:validation:Required
	Console PortRef `json:"console"`
}

// ConsoleCableEndStatus is the state of one end of a line, written by the node-agent that owns the line.
type ConsoleCableEndStatus struct {
	// NodeName is the node the device of this end runs on.
	NodeName string `json:"nodeName,omitempty"`
	// Attached: the line owner holds a running attach to the port keeper of this end.
	Attached bool `json:"attached,omitempty"`
}

// Reasons of the Ready condition of a ConsoleCable.
const (
	ConsoleCableReasonWaitingForDevice = "WaitingForDevice"
	ConsoleCableReasonPortDown         = "PortDown"
	ConsoleCableReasonPeerUnreachable  = "PeerUnreachable"
	// ConsoleCableReasonNodeLineLimit: the node of the console end runs the most lines it may (500, shared fairly between the labs).
	ConsoleCableReasonNodeLineLimit = "NodeLineLimit"
)

// ConsoleCableStatus is the observed state of a ConsoleCable (written by the node-agent of the console end).
type ConsoleCableStatus struct {
	Serial  ConsoleCableEndStatus `json:"serial,omitempty"`
	Console ConsoleCableEndStatus `json:"console,omitempty"`
	// LineUp: both ends are attached and the relay runs.
	LineUp bool `json:"lineUp,omitempty"`
	// Conditions: Ready with the reasons WaitingForDevice, PortDown and PeerUnreachable.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ConsoleCableRef summarises a ConsoleCable in the status of its Lab.
type ConsoleCableRef struct {
	Name    string  `json:"name"`
	Serial  PortRef `json:"serial"`
	Console PortRef `json:"console"`
	LineUp  bool    `json:"lineUp,omitempty"`
	// Reason is the reason of the cable's Ready condition while the line is down.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=".spec.serial.device"
// +kubebuilder:printcolumn:name="Console",type=string,JSONPath=".spec.console.device"
// +kubebuilder:printcolumn:name="Line",type=boolean,JSONPath=".status.lineUp"

// ConsoleCable is one console line of a Lab: made by the operator from Lab.spec.consoleCables and owned by the Lab. It is not a
// Connection: it never reaches Open vSwitch and has no VNI.
type ConsoleCable struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConsoleCableSpec   `json:"spec,omitempty"`
	Status ConsoleCableStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConsoleCableList contains a list of ConsoleCable.
type ConsoleCableList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConsoleCable `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConsoleCable{}, &ConsoleCableList{})
}

// HasPorts says the device declares a console port or a serial port.
func (s *DeviceSpec) HasPorts() bool { return len(s.ConsolePorts) > 0 || len(s.SerialPorts) > 0 }

// HasPorts says the device template declares a console port or a serial port.
func (t *DeviceTemplate) HasPorts() bool { return len(t.ConsolePorts) > 0 || len(t.SerialPorts) > 0 }

// UsesConsoleCables says the lab declares a port on a device or a console cable: what the consoleCables switch of the chart gates.
func (s *LabSpec) UsesConsoleCables() bool {
	if len(s.ConsoleCables) > 0 {
		return true
	}
	for i := range s.Devices {
		if s.Devices[i].HasPorts() {
			return true
		}
	}
	return false
}
