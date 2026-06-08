package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabGroupSpec defines the desired state of LabGroup.
type LabGroupSpec struct {
	VPN LabGroupVPNSpec `json:"vpn,omitempty"`
}

// LabGroupVPNSpec holds VPN server configuration.
type LabGroupVPNSpec struct {
	// KeypairSecretRef points to an existing WireGuard keypair Secret.
	// If omitted, operator generates a keypair and stores it in Secret vpn-server-keypair.
	KeypairSecretRef *corev1.SecretReference `json:"keypairSecretRef,omitempty"`
	// ClientSubnet is the WireGuard address pool for VPN clients (e.g. "10.8.0.0/24").
	ClientSubnet string `json:"clientSubnet,omitempty"`
	// Supernet is the larger network that encompasses all lab subnets (e.g. "10.8.0.0/16").
	Supernet string `json:"supernet,omitempty"`
}

// LabGroupStatus defines the observed state of LabGroup.
type LabGroupStatus struct {
	Phase     Phase             `json:"phase,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	VPN       LabGroupVPNStatus `json:"vpn,omitempty"`
}

// LabGroupVPNStatus exposes VPN server connection details.
type LabGroupVPNStatus struct {
	// PublicKey is the WireGuard public key used as routing key for demux.
	PublicKey string `json:"publicKey,omitempty"`
	// Endpoint is the public UDP address of the VPN server (host:port) advertised to clients.
	Endpoint string `json:"endpoint,omitempty"`
	// Backend is the in-cluster pod address (podIP:port) of the VPN server, used by the demux proxy.
	// Set by the controller from the VPN pod's IP; empty when no ready replica exists.
	Backend string `json:"backend,omitempty"`
	// SecretRef is "<namespace>/<name>" of the vpn-server-keypair Secret.
	SecretRef string `json:"secretRef,omitempty"`
	// Registered is true when the VPN deployment has at least one ready replica.
	Registered bool `json:"registered,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="!(self.metadata.name in ['default','kube-system','kube-public','kube-node-lease','laboratory-system'])",message="LabGroup name conflicts with a reserved Kubernetes namespace"

// LabGroup is the Schema for the labgroups API.
type LabGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LabGroupSpec   `json:"spec,omitempty"`
	Status LabGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LabGroupList contains a list of LabGroup.
type LabGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabGroup{}, &LabGroupList{})
}
