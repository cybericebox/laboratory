package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabGroupSpec defines the desired state of LabGroup.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.lifecycle) || has(self.lifecycle)",message="explicit group lifecycle intent cannot be removed"
type LabGroupSpec struct {
	// Lifecycle is independent of legacy suspension; absent means Running.
	// +optional
	Lifecycle *GroupLifecycleSpec `json:"lifecycle,omitempty"`
	// Admission serializes child creates/starts across agent replicas.
	// +optional
	Admission *GroupChildAdmission `json:"admission,omitempty"`
	VPN       LabGroupVPNSpec      `json:"vpn,omitempty"`
	// Gateway holds the internet gateway pod's settings.
	Gateway   LabGroupGatewaySpec `json:"gateway,omitempty"`
	Suspended bool                `json:"suspended,omitempty"`
}

// GroupPodSize is the size of one pod a group runs itself (the VPN or the gateway): requests = limits, so the pod is Guaranteed.
// The platform computes it when it plans the group and the pod is created at exactly that size and never resized.
type GroupPodSize struct {
	// +kubebuilder:validation:Minimum=1
	CPUMillicores int64 `json:"cpuMillicores"`
	// +kubebuilder:validation:Minimum=1
	MemoryBytes int64 `json:"memoryBytes"`
}

// LabGroupGatewaySpec holds the internet gateway configuration.
type LabGroupGatewaySpec struct {
	// Enabled is the switch of the group's internet gateway pod: the pod exists only while it is on. A lab with an Internet gateway
	// (spec.gateways, kind internet) is refused while it is off, and it cannot be turned off while a lab of the group has one. Absent
	// (a group made before the field) means on, so existing groups keep their gateway pod. The agent writes it explicitly, off unless the
	// caller asks for it.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Size is the size of the gateway pod; absent = the chart's default. Fixed once set.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="the size of a group's pod is fixed"
	// +optional
	Size *GroupPodSize `json:"size,omitempty"`
}

// LabGroupVPNSpec holds VPN server configuration.
type LabGroupVPNSpec struct {
	// Enabled is the switch of the group's VPN server pod: the pod exists only while it is on. A lab with a VPN gateway (spec.gateways,
	// kind vpn) and a LabGroupClient are refused while it is off, and it cannot be turned off while a lab of the group has a VPN
	// gateway. Absent (a group made before the field) it follows Disabled, so existing groups keep their VPN pod unless they disabled it.
	// The agent writes it explicitly (and keeps Disabled equal to its opposite), off unless the caller asks for it.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Disabled is the old switch, kept so a group made before Enabled means the same: true stops the VPN server (no pod). Enabled wins
	// when it is set. New code reads VPNEnabled(), not this field.
	Disabled bool `json:"disabled,omitempty"`
	// Deprecated: suspension now always keeps WireGuard and the internet
	// gateway running while stopping only task devices.
	ProbeWhileSuspended bool `json:"probeWhileSuspended,omitempty"`
	// KeypairSecretRef points to an existing WireGuard keypair Secret.
	// If omitted, operator generates a keypair and stores it in Secret vpn-server-keypair.
	KeypairSecretRef *corev1.SecretReference `json:"keypairSecretRef,omitempty"`
	// Size is the size of the VPN pod; absent = the chart's default. Fixed once set.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="the size of a group's pod is fixed"
	// +optional
	Size *GroupPodSize `json:"size,omitempty"`
}

// LabGroupStatus defines the observed state of LabGroup.
type LabGroupStatus struct {
	// Original birth metadata; bounded by actual group Lab capacity and never raw data.
	// +kubebuilder:validation:MaxItems=5000
	Creations []LabCreationReceipt `json:"creations,omitempty"`
	// Retirement is a fresh acknowledgement distinct from the original stop.
	// +optional
	Retirement *LifecycleRetirementStatus `json:"retirement,omitempty"`
	// ServiceRuntime is controller-owned durable pre-scale inventory.
	// +optional
	ServiceRuntime []OwnedRuntimeIdentity `json:"serviceRuntime,omitempty"`
	// ServiceReports is node-owned matching native observation.
	// +optional
	ServiceReports []OwnedRuntimeReport `json:"serviceReports,omitempty"`
	// Reserved observation seam for later full group stop; suspension is unchanged.
	// +optional
	Lifecycle *LabLifecycleStatus `json:"lifecycle,omitempty"`
	// +optional
	Resources *RuntimeAllocation `json:"resources,omitempty"`
	Phase     Phase              `json:"phase,omitempty"`
	Namespace string             `json:"namespace,omitempty"`
	Suspended bool               `json:"suspended,omitempty"`
	VPN       LabGroupVPNStatus  `json:"vpn,omitempty"`
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
	// Backend is "<pod IP>:<port>", the UDP address of the ready VPN pod of the group, written by the operator on every change of that pod;
	// the demux sends the group's handshakes there and uses no name. Empty while no VPN pod is ready: the demux then drops them.
	Backend string `json:"backend,omitempty"`
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

// VPNEnabled says whether the group runs its VPN server: spec.vpn.enabled when set, else the opposite of the old spec.vpn.disabled.
func (g *LabGroup) VPNEnabled() bool {
	if g.Spec.VPN.Enabled != nil {
		return *g.Spec.VPN.Enabled
	}
	return !g.Spec.VPN.Disabled
}

// InternetEnabled says whether the group runs its internet gateway: spec.gateway.enabled when set, else on (a group made before the
// switch has its gateway pod).
func (g *LabGroup) InternetEnabled() bool {
	return g.Spec.Gateway.Enabled == nil || *g.Spec.Gateway.Enabled
}
