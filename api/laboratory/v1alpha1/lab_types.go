package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabSpec defines the desired state of Lab.
type LabSpec struct {
	VPN         LabNetworkSpec       `json:"vpn,omitempty"`
	Internet    LabNetworkSpec       `json:"internet,omitempty"`
	Devices     []DeviceTemplate     `json:"devices,omitempty"`
	Connections []ConnectionTemplate `json:"connections,omitempty"`
}

// LabNetworkSpec configures a network segment (VPN or internet) attached to the lab.
type LabNetworkSpec struct {
	Enabled    bool        `json:"enabled,omitempty"`
	DHCPServer *DHCPServer `json:"dhcpServer,omitempty"`
}

// DHCPServer enables the embedded DHCP server for a network segment.
// Subnet and gateway are derived from the lab's allocated CIDR (Status.*.CIDR).
type DHCPServer struct {
	Enabled bool        `json:"enabled,omitempty"`
	Ranges  []DHCPRange `json:"ranges,omitempty"`
	// DNS is advertised only by the internet gateway's DHCP server.
	DNS string `json:"dns,omitempty"`
}

// DHCPRange is an inclusive host-offset interval within the lab /24.
// An enabled DHCP server must have at least one range.
type DHCPRange struct {
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=254
	Start int32 `json:"start"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=254
	End int32 `json:"end"`
}

// DeviceTemplate is an inline device declaration inside Lab.spec.devices[].
type DeviceTemplate struct {
	// Name becomes part of the lab's web address (<name>-<code>.<domain>), so
	// it is a DNS label of at most 35 characters (names.MaxDeviceNameLen).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=35
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Type  DeviceType `json:"type"`
	Image string     `json:"image,omitempty"`
	// SecurityPreset names a capability profile for the device container. Only
	// the preset name is exposed here; the concrete Linux capabilities behind it
	// are an internal platform decision. Empty means "basic" (no extra caps).
	SecurityPreset SecurityPreset  `json:"securityPreset,omitempty"`
	Interfaces     []InterfaceSpec `json:"interfaces,omitempty"`
	Exposure       *ExposureSpec   `json:"exposure,omitempty"`
	// Resources sets the container resource requests/limits for this device.
	// +optional
	Resources *DeviceResources `json:"resources,omitempty"`
}

// ConnectionTemplate is an inline connection declaration inside Lab.spec.connections[].
type ConnectionTemplate struct {
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Endpoints []EndpointSpec `json:"endpoints"`
}

// LabStatus defines the observed state of Lab.
type LabStatus struct {
	Phase       Phase            `json:"phase,omitempty"`
	VPN         LabNetworkStatus `json:"vpn,omitempty"`
	Internet    LabNetworkStatus `json:"internet,omitempty"`
	Devices     []DeviceRef      `json:"devices,omitempty"`
	Connections []ConnectionRef  `json:"connections,omitempty"`
	Access      []AccessEntry    `json:"access,omitempty"`
	// StatePersistence records how this lab runs its devices, decided once on the
	// first reconcile: true = bare Pods with snapshot-backed state, false =
	// Deployments. It never changes afterwards, whatever the platform switch says.
	// +optional
	StatePersistence *bool `json:"statePersistence,omitempty"`
	// Conditions surfaces reconciler progress/blocking reasons
	// (e.g. Ready=False reason=WaitingForInterface) for kubectl and clients.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// LabNetworkStatus reports the allocated CIDR for a network segment.
type LabNetworkStatus struct {
	Ready bool   `json:"ready,omitempty"`
	CIDR  string `json:"cidr,omitempty"`
}

// DeviceRef summarises a materialised Device's readiness.
type DeviceRef struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready,omitempty"`
	// State summarises the snapshots of a device with state persistence.
	// +optional
	State *DeviceStateInfo `json:"state,omitempty"`
}

// DeviceStateInfo is the organizer-facing view of a device's snapshots.
type DeviceStateInfo struct {
	LastSnapshotAt *metav1.Time `json:"lastSnapshotAt,omitempty"`
	RestoredAt     *metav1.Time `json:"restoredAt,omitempty"`
	SizeBytes      int64        `json:"sizeBytes,omitempty"`
	// QuotaWarning is set while snapshots are refused or failing.
	QuotaWarning string `json:"quotaWarning,omitempty"`
	// Rescue is true while the device runs in rescue mode.
	Rescue bool `json:"rescue,omitempty"`
}

// ConnectionRef summarises a materialised Connection's readiness.
type ConnectionRef struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready,omitempty"`
}

// AccessEntry describes a single externally reachable endpoint.
type AccessEntry struct {
	Device   string `json:"device"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Lab is the Schema for the labs API.
type Lab struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LabSpec   `json:"spec,omitempty"`
	Status LabStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LabList contains a list of Lab.
type LabList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Lab `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lab{}, &LabList{})
}
