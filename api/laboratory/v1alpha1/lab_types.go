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
	Enabled bool `json:"enabled,omitempty"`
}

// DeviceTemplate is an inline device declaration inside Lab.spec.devices[].
type DeviceTemplate struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Type       DeviceType      `json:"type"`
	Image      string          `json:"image,omitempty"`
	Interfaces []InterfaceSpec `json:"interfaces,omitempty"`
	Exposure   *ExposureSpec   `json:"exposure,omitempty"`
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
