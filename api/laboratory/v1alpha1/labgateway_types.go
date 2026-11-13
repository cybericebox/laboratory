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
	// Device is the name of the gateway of the lab this object serves (spec.gateways[].name).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Device string `json:"device"`
	// NetworkIndex is the number of the gateway's leg (interface lab<N>), from the lab-subnets pool. The CIDR is not derived from
	// it: it is in the parent Lab status (status.gateways). Immutable after FinalizerController is set.
	NetworkIndex uint `json:"networkIndex,omitempty"`
}

// LabGatewayStatus defines the observed state of LabGateway.
type LabGatewayStatus struct {
	Phase       LabGatewayPhase `json:"phase,omitempty"`
	NATReady    bool            `json:"natReady,omitempty"`
	DHCPEnabled bool            `json:"dhcpEnabled,omitempty"`
	DHCPReady   bool            `json:"dhcpReady,omitempty"`
	// Conditions surfaces reconciler progress/blocking reasons
	// (e.g. Ready=False reason=WaitingForInterface) for kubectl and clients.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
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
