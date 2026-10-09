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

// VPNRuntimeIdentity binds an acknowledgement to one process boot and native
// container identity. Empty identities cannot certify a current access fence.
type VPNRuntimeIdentity struct {
	BootID       string `json:"bootId"`
	PodName      string `json:"podName"`
	PodUID       string `json:"podUID"`
	ContainerID  string `json:"containerID"`
	RestartCount int32  `json:"restartCount"`
}

// LabAccessFence is written only after both-direction rules and conntrack
// retirement succeed for this exact live Lab operation.
type LabAccessFence struct {
	OperationID        string `json:"operationId"`
	Revision           int64  `json:"revision"`
	LabUID             string `json:"labUID"`
	ObservedGeneration int64  `json:"observedGeneration"`
	GroupUID           string `json:"groupUID"`
	VPNRuntimeIdentity `json:",inline"`
	FencedAt           metav1.Time `json:"fencedAt"`
}

// LabVPNStatus defines the observed state of LabVPN.
type LabVPNStatus struct {
	// Runtime is published at process startup before any kernel reconciliation.
	Runtime     *VPNRuntimeIdentity `json:"runtime,omitempty"`
	AccessFence *LabAccessFence     `json:"accessFence,omitempty"`
	Phase       LabVPNPhase         `json:"phase,omitempty"`
	DHCPEnabled bool                `json:"dhcpEnabled,omitempty"`
	DHCPReady   bool                `json:"dhcpReady,omitempty"`
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

// VPNBootRecord is the independent startup witness, including idle groups with
// no per-Lab VPN legs. It is separate from policy and traffic acknowledgements.
type VPNBootRecord struct {
	VPNRuntimeIdentity `json:",inline"`
	GroupUID           string      `json:"groupUID"`
	PublishedAt        metav1.Time `json:"publishedAt"`
}
