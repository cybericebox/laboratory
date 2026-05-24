package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabGroupClientSpec defines the desired state of LabGroupClient.
type LabGroupClientSpec struct {
	// PublicKey is the client's WireGuard public key.
	// If empty, operator generates a keypair and stores private key in Secret client-<name>.
	PublicKey string `json:"publicKey,omitempty"`
}

// LabGroupClientStatus defines the observed state of LabGroupClient.
type LabGroupClientStatus struct {
	// AssignedIP is the VPN tunnel IP in CIDR notation, e.g. 10.8.0.5/32.
	AssignedIP string `json:"assignedIP,omitempty"`
	// SecretRef is the name of the Secret in the same namespace holding connection config.
	SecretRef  string                   `json:"secretRef,omitempty"`
	Statistics LabGroupClientStatistics `json:"statistics,omitempty"`
}

// LabGroupClientStatistics is written periodically by the VPN server.
type LabGroupClientStatistics struct {
	LastHandshake metav1.Time `json:"lastHandshake,omitempty"`
	RxBytes       int64       `json:"rxBytes,omitempty"`
	TxBytes       int64       `json:"txBytes,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// LabGroupClient is the Schema for the labgroupclients API.
type LabGroupClient struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LabGroupClientSpec   `json:"spec,omitempty"`
	Status LabGroupClientStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LabGroupClientList contains a list of LabGroupClient.
type LabGroupClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabGroupClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabGroupClient{}, &LabGroupClientList{})
}
