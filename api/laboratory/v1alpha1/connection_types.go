package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConnectionSpec defines the desired state of Connection.
type ConnectionSpec struct {
	// LabRef is the name of the parent Lab.
	// +kubebuilder:validation:Required
	LabRef string `json:"labRef"`
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Endpoints []EndpointSpec `json:"endpoints"`
}

// ConnectionStatus defines the observed state of Connection.
type ConnectionStatus struct {
	// VNI is the allocated VXLAN Network Identifier for direct (non-switch) connections.
	// Nil for switch-to-device and switch-to-switch connections.
	VNILease *VNILease              `json:"vniLease,omitempty"`
	VNI      *uint                  `json:"vni,omitempty"`
	Ports    []ConnectionPortStatus `json:"ports,omitempty"`
	Ready    bool                   `json:"ready,omitempty"`
	// Conditions surfaces reconciler progress/blocking reasons
	// (e.g. Ready=False reason=WaitingForInterface) for kubectl and clients.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ConnectionPortStatus is written by the node-agent for each endpoint.
type ConnectionPortStatus struct {
	PodUID    string `json:"podUID,omitempty"`
	RowUUID   string `json:"rowUUID,omitempty"`
	Device    string `json:"device"`
	Interface string `json:"interface,omitempty"`
	// PortID is the OVS port name assigned by node-agent.
	PortID      string `json:"portID,omitempty"`
	NodeName    string `json:"nodeName,omitempty"`
	NodeAddress string `json:"nodeAddress,omitempty"`
	Connected   bool   `json:"connected,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Connection is the Schema for the connections API.
type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionSpec   `json:"spec,omitempty"`
	Status ConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectionList contains a list of Connection.
type ConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Connection{}, &ConnectionList{})
}
