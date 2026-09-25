package v1alpha1

// LabGroupNamespace returns the Kubernetes namespace for a LabGroup.
// The namespace is identical to the group name — no prefix — so users can
// derive it trivially: group "team-alpha" → namespace "team-alpha".
func LabGroupNamespace(groupName string) string {
	return groupName
}

// Phase is the lifecycle phase of a resource.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Suspended;Failed;Error
type Phase string

const (
	PhasePending      Phase = "Pending"
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

// SecurityPreset selects a named capability profile for a device container.
// The concrete Linux capabilities behind each preset are resolved internally by
// the operator and are intentionally NOT part of the public spec, so the
// requirement surface stays hidden and can move behind a custom agent later.
//   - basic:   no extra capabilities (a plain service).
//   - service: bind privileged ports.
//   - net:     networking/testing tools (ping, tcpdump, ip, iptables, DHCP).
//   - debug:   net plus process debugging (gdb/strace).
//
// +kubebuilder:validation:Enum=basic;service;net;debug
type SecurityPreset string

const (
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
	// Interface is the interface name on the device; omit for unmanaged-switch/hub.
	Interface string `json:"interface,omitempty"`
}

// InterfaceSpec defines a network interface on a device.
type InterfaceSpec struct {
	// +kubebuilder:validation:Required
	Name string   `json:"name"`
	Addr AddrSpec `json:"addr,omitempty"`
	// MAC is "random" or an explicit MAC address.
	MAC string `json:"mac,omitempty"`
}

// AddrSpec defines static or DHCP address configuration.
type AddrSpec struct {
	// +kubebuilder:validation:Required
	Type    AddrType `json:"type"`
	IP      string   `json:"ip,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	Routes  []Route  `json:"routes,omitempty"`
}

// Route is a static route entry.
type Route struct {
	// +kubebuilder:validation:Required
	Dst string `json:"dst"`
	Via string `json:"via,omitempty"`
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
