package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabGroupClientSpec defines the desired state of LabGroupClient.
// +kubebuilder:validation:XValidation:rule="(has(self.accessOnly) && self.accessOnly) == (has(oldSelf.accessOnly) && oldSelf.accessOnly)",message="accessOnly cannot be changed"
// +kubebuilder:validation:XValidation:rule="!has(self.accessOnly) || !self.accessOnly || !has(self.publicKey) || self.publicKey == ''",message="an access-only client has no public key"
type LabGroupClientSpec struct {
	// AccessOnly makes the client a name for the access policy and the L7 proxy only: it has no WireGuard peer, no tunnel address and no
	// config, and the operator allocates nothing for it. The proxy lets such a client in (the gRPC access of an AI opponent, whose control
	// is out-of-band) when the group policy allows it, exactly as for a VPN client. Fixed at creation.
	// +optional
	AccessOnly bool `json:"accessOnly,omitempty"`
	// PublicKey is the client's WireGuard public key.
	// If empty, operator generates a keypair and stores private key in Secret client-<name>.
	PublicKey string `json:"publicKey,omitempty"`
}

// LabGroupClientStatus defines the observed state of LabGroupClient.
type LabGroupClientStatus struct {
	// AssignedIP is the VPN tunnel IP in CIDR notation, e.g. 10.8.0.5/32.
	AssignedIP string `json:"assignedIP,omitempty"`
	// Config is the assembled WireGuard client config, with a placeholder
	// (names.WGPrivateKeyPlaceholder) where the private key goes — the cluster
	// never holds the private key. The caller substitutes its own private key.
	Config     string                   `json:"config,omitempty"`
	Statistics LabGroupClientStatistics `json:"statistics,omitempty"`
	// Conditions surfaces reconciler progress/blocking reasons
	// (e.g. Ready=False reason=WaitingForInterface) for kubectl and clients.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
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
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="LabGroupClient name must be at most 63 characters"

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
