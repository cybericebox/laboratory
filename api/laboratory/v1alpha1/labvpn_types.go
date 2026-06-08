package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabVPNPhase is the lifecycle phase of a LabVPN.
// +kubebuilder:validation:Enum=Pending;WaitingForInterface;Configuring;Ready
type LabVPNPhase string

const (
	LabVPNPhasePending             LabVPNPhase = "Pending"
	LabVPNPhaseWaitingForInterface LabVPNPhase = "WaitingForInterface"
	LabVPNPhaseConfiguring         LabVPNPhase = "Configuring"
	LabVPNPhaseReady               LabVPNPhase = "Ready"
)

// LabVPNSpec defines the desired state of LabVPN.
type LabVPNSpec struct {
	// LabName is the name of the parent Lab.
	// +kubebuilder:validation:Required
	LabName string `json:"labName"`
	// NetworkIndex is the subnet index N from the lab-subnets pool.
	// VPN CIDR is derived as 10.8.N.0/24. Immutable after FinalizerController is set.
	NetworkIndex uint `json:"networkIndex,omitempty"`
}

// LabVPNStatus defines the observed state of LabVPN.
type LabVPNStatus struct {
	Phase       LabVPNPhase `json:"phase,omitempty"`
	DHCPEnabled bool        `json:"dhcpEnabled,omitempty"`
	DHCPReady   bool        `json:"dhcpReady,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// LabVPN is the Schema for the labvpns API.
type LabVPN struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LabVPNSpec   `json:"spec,omitempty"`
	Status LabVPNStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LabVPNList contains a list of LabVPN.
type LabVPNList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabVPN `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabVPN{}, &LabVPNList{})
}
