package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// LabGroupNamespacePrefix starts the name of the namespace of every NEW LabGroup. A group name is chosen by a
// tenant; with a fixed prefix it can never equal a namespace that already exists (kube-system, laboratory-system,
// laboratory-tenants, another tenant's group...), so the operator never adopts or deletes a foreign namespace.
const LabGroupNamespacePrefix = "lg-"

// LabGroupNamespace returns the namespace the operator creates for a NEW LabGroup of this name:
// "lg-" + the name (shortened to 40 characters) + "-" + 8 hex digits of the SHA-256 of the whole name, at most 52
// characters. Groups created before the prefix existed keep the namespace in their status (see LabGroupNamespaceOf).
func LabGroupNamespace(groupName string) string {
	sum := sha256.Sum256([]byte(groupName))
	short := groupName
	if len(short) > 40 {
		short = short[:40]
	}
	short = strings.TrimRight(short, "-")
	return LabGroupNamespacePrefix + short + "-" + hex.EncodeToString(sum[:4])
}

// LabGroupNamespaceOf is the namespace of an existing LabGroup: the one recorded in its status (so a group that
// was created under the old naming, where the namespace was the bare group name, keeps working), else the name a
// new group gets.
func LabGroupNamespaceOf(lg *LabGroup) string {
	if lg.Status.Namespace != "" {
		return lg.Status.Namespace
	}
	return LabGroupNamespace(lg.Name)
}

// Phase is the lifecycle phase of a resource.
// +kubebuilder:validation:Enum=Pending;Queued;Provisioning;Ready;Suspended;Failed;Error
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseQueued       Phase = "Queued"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseSuspended    Phase = "Suspended"
	PhaseFailed       Phase = "Failed"
	PhaseError        Phase = "Error"
)

// DeviceType distinguishes network device kinds.
// +kubebuilder:validation:Enum=container;unmanaged-switch;hub
type DeviceType string

const (
	DeviceTypeContainer       DeviceType = "container"
	DeviceTypeUnmanagedSwitch DeviceType = "unmanaged-switch"
	DeviceTypeHub             DeviceType = "hub"
)

// AddrType controls how an interface gets its address.
//   - static:      fixed IP from spec; applied by the netconfig init-container.
//   - dhcp:        the image runs its own DHCP client; the device pod is granted
//     NET_ADMIN+NET_RAW so that client can lease and configure the interface.
//   - dhcp-preset: the image has no DHCP client but still needs a dynamic
//     address; the netconfig init-container leases one from the lab DHCP server
//     and applies it. The device container needs no capabilities.
//
// +kubebuilder:validation:Enum=static;dhcp;dhcp-preset
type AddrType string

const (
	AddrTypeStatic     AddrType = "static"
	AddrTypeDHCP       AddrType = "dhcp"
	AddrTypeDHCPPreset AddrType = "dhcp-preset"
)

// SecurityPreset selects a device security profile from the fixed catalog of the laboratory (internal/profiles). The
// concrete Linux capabilities behind a profile are resolved by the operator and are intentionally NOT part of the
// public spec. Empty means standard.
//   - standard: the base set plus SYS_PTRACE, IPC_LOCK, LINUX_IMMUTABLE; ping through ping_group_range. Web, API, databases,
//     SSH, privilege escalation, cracking, forensics, gdb and strace.
//   - extended: standard plus NET_RAW, NET_ADMIN and /dev/net/tun. Raw scans, sniffing, spoofing, routers, VPNs, tunnels.
//
// The old names stay accepted as aliases: basic and service mean standard, net and debug mean extended.
//
// +kubebuilder:validation:Enum=standard;extended;basic;service;net;debug
type SecurityPreset string

const (
	SecurityPresetStandard SecurityPreset = "standard"
	SecurityPresetExtended SecurityPreset = "extended"
	// Deprecated aliases.
	SecurityPresetBasic   SecurityPreset = "basic"
	SecurityPresetService SecurityPreset = "service"
	SecurityPresetNet     SecurityPreset = "net"
	SecurityPresetDebug   SecurityPreset = "debug"
)

// EndpointSpec references a device interface in a Connection.
type EndpointSpec struct {
	// Device is the name of the Device CRD within the same Lab.
	// +kubebuilder:validation:Required
	Device string `json:"device"`
	// Interface is a declared interface on a container or a logical
	// GigabitEthernet0/1..GigabitEthernet0/48 port on a switch/hub.
	Interface string `json:"interface,omitempty"`
}

// InterfaceSpec defines a network interface on a device.
type InterfaceSpec struct {
	// Name is the interface name inside the pod: a lowercase word of at most 15
	// characters; lo and accessport are reserved.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,14}$`
	// +kubebuilder:validation:XValidation:rule="self != 'lo' && self != 'accessport'",message="reserved interface name"
	Name string   `json:"name"`
	Addr AddrSpec `json:"addr,omitempty"`
	// MAC is "random" or an explicit unicast MAC address.
	// +kubebuilder:validation:MaxLength=17
	// +kubebuilder:validation:Pattern=`^(random|[0-9a-fA-F][02468aceACE](:[0-9a-fA-F]{2}){5})$`
	// +kubebuilder:validation:XValidation:rule="self != '00:00:00:00:00:00'",message="the zero MAC is not allowed"
	MAC string `json:"mac,omitempty"`
}

// AddrSpec defines static or DHCP address configuration.
type AddrSpec struct {
	// +kubebuilder:validation:Required
	Type       AddrType      `json:"type"`
	IP         string        `json:"ip,omitempty"`
	AddressRef *NetworkIPRef `json:"addressRef,omitempty"`
	Gateway    string        `json:"gateway,omitempty"`
	GatewayRef *NetworkIPRef `json:"gatewayRef,omitempty"`
	Routes     []Route       `json:"routes,omitempty"`
}

// NetworkIPRef selects a host address from a lab-allocated VPN or Internet /24.
// The lab reconciler resolves it before creating a Device.
type NetworkIPRef struct {
	// +kubebuilder:validation:Enum=vpn;internet
	Network string `json:"network"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=254
	Host int32 `json:"host"`
}

// NetworkSubnetRef selects the full lab-allocated VPN or Internet /24.
type NetworkSubnetRef struct {
	// +kubebuilder:validation:Enum=vpn;internet
	Network string `json:"network"`
}

// Route is a static route entry.
type Route struct {
	Dst    string            `json:"dst,omitempty"`
	DstRef *NetworkSubnetRef `json:"dstRef,omitempty"`
	Via    string            `json:"via,omitempty"`
	ViaRef *NetworkIPRef     `json:"viaRef,omitempty"`
}

// ExposureSpec declares how a device is externally reachable.
type ExposureSpec struct {
	Web *WebExposure `json:"web,omitempty"`
}

// WebExposure publishes a device port via an L7 proxy.
type WebExposure struct {
	// +kubebuilder:validation:Required
	Port int32 `json:"port"`
	// Protocol is "http" or "https".
	// +kubebuilder:default=http
	Protocol string `json:"protocol,omitempty"`
}
