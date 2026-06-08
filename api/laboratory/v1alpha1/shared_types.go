package v1alpha1

// LabGroupNamespace returns the Kubernetes namespace for a LabGroup.
// The namespace is identical to the group name — no prefix — so users can
// derive it trivially: group "team-alpha" → namespace "team-alpha".
func LabGroupNamespace(groupName string) string {
	return groupName
}

// Phase is the lifecycle phase of a resource.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;Error
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseFailed       Phase = "Failed"
	PhaseError        Phase = "Error"
)

// DeviceType distinguishes network device kinds.
// +kubebuilder:validation:Enum=container;vm;unmanaged-switch;hub
type DeviceType string

const (
	DeviceTypeContainer       DeviceType = "container"
	DeviceTypeVM              DeviceType = "vm"
	DeviceTypeUnmanagedSwitch DeviceType = "unmanaged-switch"
	DeviceTypeHub             DeviceType = "hub"
)

// AddrType controls how an interface gets its address.
// +kubebuilder:validation:Enum=static;dhcp
type AddrType string

const (
	AddrTypeStatic AddrType = "static"
	AddrTypeDHCP   AddrType = "dhcp"
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
