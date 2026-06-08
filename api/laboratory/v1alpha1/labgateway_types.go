package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabGatewayPhase is the lifecycle phase of a LabGateway.
// +kubebuilder:validation:Enum=Pending;WaitingForInterface;Configuring;Ready
type LabGatewayPhase string

const (
	LabGatewayPhasePending             LabGatewayPhase = "Pending"
	LabGatewayPhaseWaitingForInterface LabGatewayPhase = "WaitingForInterface"
	LabGatewayPhaseConfiguring         LabGatewayPhase = "Configuring"
	LabGatewayPhaseReady               LabGatewayPhase = "Ready"
)

// LabGatewaySpec defines the desired state of LabGateway.
type LabGatewaySpec struct {
	// LabName is the name of the parent Lab.
	// +kubebuilder:validation:Required
	LabName string `json:"labName"`
	// NetworkIndex is the subnet index N from the lab-subnets pool.
	// Internet CIDR is derived as 10.9.N.0/24. Immutable after FinalizerController is set.
	NetworkIndex uint `json:"networkIndex,omitempty"`
}

// LabGatewayStatus defines the observed state of LabGateway.
type LabGatewayStatus struct {
	Phase       LabGatewayPhase `json:"phase,omitempty"`
	NATReady    bool            `json:"natReady,omitempty"`
	DHCPEnabled bool            `json:"dhcpEnabled,omitempty"`
	DHCPReady   bool            `json:"dhcpReady,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// LabGateway is the Schema for the labgateways API.
type LabGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LabGatewaySpec   `json:"spec,omitempty"`
	Status LabGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LabGatewayList contains a list of LabGateway.
type LabGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabGateway{}, &LabGatewayList{})
}
