package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabGroupSpec defines the desired state of LabGroup.
type LabGroupSpec struct {
	VPN       LabGroupVPNSpec `json:"vpn,omitempty"`
	Suspended bool            `json:"suspended,omitempty"`
}

// LabGroupVPNSpec holds VPN server configuration.
type LabGroupVPNSpec struct {
	// Disabled stops only the VPN server; internet gateway and Labs remain running.
	Disabled bool `json:"disabled,omitempty"`
	// Deprecated: suspension now always keeps WireGuard and the internet
	// gateway running while stopping only task devices.
	ProbeWhileSuspended bool `json:"probeWhileSuspended,omitempty"`
	// KeypairSecretRef points to an existing WireGuard keypair Secret.
	// If omitted, operator generates a keypair and stores it in Secret vpn-server-keypair.
	KeypairSecretRef *corev1.SecretReference `json:"keypairSecretRef,omitempty"`
}

// LabGroupStatus defines the observed state of LabGroup.
type LabGroupStatus struct {
	Phase     Phase             `json:"phase,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Suspended bool              `json:"suspended,omitempty"`
	VPN       LabGroupVPNStatus `json:"vpn,omitempty"`
	// ImageWarning names the VPN or gateway image that could not be pinned to a
	// digest when the group's pods were created with the image cache on; the pod
	// pulls it by tag. Empty when all were pinned (or the cache was off).
	// +optional
	ImageWarning string `json:"imageWarning,omitempty"`
	// Scheduling is the place of the group in the scheduler queue.
	// +optional
	Scheduling *SchedulingStatus `json:"scheduling,omitempty"`
	// Pods is the scheduling state of the group's own pods ("vpn", "gateway").
	// Absent for a group that predates the scheduler: its pods are not queued.
	// +optional
	// +listType=map
	// +listMapKey=name
	Pods []NamedPodSchedule `json:"pods,omitempty"`
}

// LabGroupVPNStatus exposes VPN server connection details.
type LabGroupVPNStatus struct {
	// ClientSubnet is the group subnet containing the tunnel-only test gateway.
	ClientSubnet string `json:"clientSubnet,omitempty"`
	// PublicKey is the WireGuard public key used as routing key for demux.
	PublicKey string `json:"publicKey,omitempty"`
	// Endpoint is the public UDP address of the VPN server (host:port) advertised to clients.
	Endpoint string `json:"endpoint,omitempty"`
	// SecretRef is "<namespace>/<name>" of the vpn-server-keypair Secret.
	SecretRef string `json:"secretRef,omitempty"`
	// Registered is true when the VPN deployment has at least one ready replica,
	// including while Lab devices are suspended.
	Registered bool `json:"registered,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="LabGroup name must be at most 63 characters"

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
