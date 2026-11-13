package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabSpec defines the desired state of Lab.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.lifecycle) || has(self.lifecycle)",message="explicit lifecycle intent cannot be removed"
type LabSpec struct {
	// Lifecycle is absent for legacy running laboratories.
	// +optional
	Lifecycle *LabLifecycleSpec `json:"lifecycle,omitempty"`
	// Gateways are the VPN entries and Internet exits of the lab. Each one is a device of its own kind with one port, eth0, attached to a
	// lab network by an ordinary connection (endpoint device = the gateway name); it has its own /24, its DHCP settings and, on a VPN
	// gateway, its own reverse-connections switch. The names share one space with the devices. The chart value operator.maxLabGateways
	// (default 10) limits their number; the agent checks it at creation and the operator again.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Gateways []GatewayTemplate `json:"gateways,omitempty"`
	// Devices of the lab, switches and hubs included. The ceiling is fixed in code (names.MaxLabDevices).
	// +kubebuilder:validation:MaxItems=64
	Devices     []DeviceTemplate     `json:"devices,omitempty"`
	Connections []ConnectionTemplate `json:"connections,omitempty"`
	// ImagePullSecret names a kubernetes.io/dockerconfigjson Secret in the lab's namespace with
	// the pull credentials of this lab's private images. Only the management agent sets it,
	// from the credentials the platform sends with the deploy; a value in a client's spec is
	// ignored.
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
	// Checks lists the checks of the lab without their definitions (those are in the Secret <lab>-checks). Only the
	// management agent sets it; a value in a client's spec is ignored.
	// +optional
	Checks *LabChecksSpec `json:"checks,omitempty"`
}

// GatewayKind is the kind of a gateway: a VPN entry or an Internet exit.
// +kubebuilder:validation:Enum=vpn;internet
type GatewayKind string

const (
	GatewayKindVPN      GatewayKind = "vpn"
	GatewayKindInternet GatewayKind = "internet"
)

// GatewayTemplate declares one VPN entry or Internet exit of a lab.
// +kubebuilder:validation:XValidation:rule="!has(self.reverseConnections) || !self.reverseConnections || self.kind == 'vpn'",message="reverseConnections belongs to a vpn gateway only"
type GatewayTemplate struct {
	// Name is a DNS label of at most 35 characters (names.MaxDeviceNameLen), unique among the devices and gateways of the lab. It is
	// part of the names of the gateway's objects.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=35
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Kind GatewayKind `json:"kind"`
	// DHCPServer is the embedded DHCP server of the gateway's /24 (DNS only on an internet gateway).
	// +optional
	DHCPServer *DHCPServer `json:"dhcpServer,omitempty"`
	// ReverseConnections (vpn gateway only) allows a device of the lab to START connections towards a VPN client; see the VPN client rules of the lab.
	// +optional
	ReverseConnections bool `json:"reverseConnections,omitempty"`
}

// GatewayStatus is the observed state of one gateway of a lab, the shorthand ones included (named vpn and internet).
type GatewayStatus struct {
	Name string      `json:"name"`
	Kind GatewayKind `json:"kind"`
	// CIDR is the /24 of the gateway, taken once from the one lab address pool of the group and kept for the life of the lab.
	CIDR string `json:"cidr,omitempty"`
	// NetworkIndex is the number of the gateway's leg (interface lab<N> in the VPN or gateway pod, and its port): the position of the
	// /24 in the lab base network.
	NetworkIndex uint `json:"networkIndex,omitempty"`
	Ready        bool `json:"ready,omitempty"`
	// LabInitiatedBlocked (vpn gateway only): the VPN gateway confirmed that it blocks connections started by the lab's devices towards VPN clients. False while the group's VPN pod runs an older program or has not applied the lab yet.
	// +optional
	LabInitiatedBlocked bool `json:"labInitiatedBlocked,omitempty"`
}

// DHCPServer enables the embedded DHCP server for a network segment.
// Subnet and gateway are derived from the lab's allocated CIDR (Status.*.CIDR).
type DHCPServer struct {
	Enabled bool        `json:"enabled,omitempty"`
	Ranges  []DHCPRange `json:"ranges,omitempty"`
	// DNS is advertised only by the internet gateway's DHCP server.
	DNS string `json:"dns,omitempty"`
}

// DHCPRange is an inclusive host-offset interval within the lab /24.
// An enabled DHCP server must have at least one range.
type DHCPRange struct {
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=254
	Start int32 `json:"start"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=254
	End int32 `json:"end"`
}

// DeviceTemplate is an inline device declaration inside Lab.spec.devices[].
// +kubebuilder:validation:XValidation:rule="self.type != 'container' || !has(self.interfaces) || size(self.interfaces) <= 16",message="a container device has at most 16 interfaces"
// +kubebuilder:validation:XValidation:rule="!has(self.profile) || self.profile == 'container' || self.type == 'container'",message="only a container device has a profile"
type DeviceTemplate struct {
	// Name becomes part of the lab's web address (<name>-<code>.<domain>), so
	// it is a DNS label of at most 35 characters (names.MaxDeviceNameLen).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=35
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="!(self in ['vpn', 'gateway', 'internet'])",message="the names vpn, gateway and internet are reserved by the platform"
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Type  DeviceType `json:"type"`
	Image string     `json:"image,omitempty"`
	// Profile is how the device runs: container (the default when empty) or microvm. vm is reserved and refused.
	// +optional
	Profile DeviceProfile `json:"profile,omitempty"`
	// SecurityPreset names a device security profile (standard or extended). Only the name is exposed here; the concrete Linux capabilities behind it
	// are an internal platform decision. Empty means standard.
	SecurityPreset SecurityPreset `json:"securityPreset,omitempty"`
	// Interfaces of the device: at most 16 on a container (names.MaxContainerInterfaces), 48 on a switch or hub.
	// +kubebuilder:validation:MaxItems=48
	Interfaces []InterfaceSpec `json:"interfaces,omitempty"`
	// Access lists how the device is reached besides its network: http (a web port behind the L7 proxy), terminal, and the reserved vnc
	// and rdp. At most one entry per kind.
	// +listType=map
	// +listMapKey=kind
	// +kubebuilder:validation:MaxItems=4
	// +optional
	Access []DeviceAccess `json:"access,omitempty"`
	// Resources sets the container resource requests/limits for this device.
	// +optional
	Resources *DeviceResources `json:"resources,omitempty"`
	// Persistence is the optional state-persistence policy of this device, set at
	// creation and immutable. The excluded paths and the quota are platform settings.
	// +optional
	Persistence *DevicePersistence `json:"persistence,omitempty"`
	// WaitReadyFile makes the device Ready only once the application has created /run/ready inside the container.
	// A port published through the http access is checked in any case, whatever this says. Container devices only.
	// +optional
	WaitReadyFile bool `json:"waitReadyFile,omitempty"`
	// Service is the service command of a container device, set by the platform from the image configuration. Empty fields
	// mean "not provided": the image decides.
	// +optional
	Service *DeviceService `json:"service,omitempty"`
	// Bootstrap is set by the management agent when it received a bootstrap list for the device; a value sent in the spec is
	// replaced. The list is in the Secret <device>-bootstrap, never here.
	// +optional
	Bootstrap *DeviceBootstrapSpec `json:"bootstrap,omitempty"`
}

// DevicePersistence is the per-device state-persistence request of a topology.
type DevicePersistence struct {
	// Enabled turns snapshot-backed state on for the device.
	Enabled bool `json:"enabled,omitempty"`
	// Debounce is how long the writable layer must stay quiet before a snapshot
	// (default: the platform setting).
	// +optional
	Debounce *metav1.Duration `json:"debounce,omitempty"`
}

// ConnectionTemplate is an inline connection declaration inside Lab.spec.connections[].
type ConnectionTemplate struct {
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Endpoints []EndpointSpec `json:"endpoints"`
}

// LabStatus defines the observed state of Lab.
type LabStatus struct {
	ScopeInventory []OwnedRuntimeIdentity `json:"scopeInventory,omitempty"`
	ScopeReports   []OwnedRuntimeReport   `json:"scopeReports,omitempty"`
	// Retirement is a fresh acknowledgement distinct from the original stop.
	// +optional
	Retirement *LifecycleRetirementStatus `json:"retirement,omitempty"`
	// +optional
	Lifecycle *LabLifecycleStatus `json:"lifecycle,omitempty"`
	// +optional
	Resources *RuntimeAllocation `json:"resources,omitempty"`
	Phase     Phase              `json:"phase,omitempty"`
	// Gateways is the state of every gateway of the lab.
	// +listType=map
	// +listMapKey=name
	// +optional
	Gateways []GatewayStatus `json:"gateways,omitempty"`
	Devices  []DeviceRef     `json:"devices,omitempty"`
	// DeviceNodes are the nodes the devices of the lab run on now (the sorted, distinct Device.status.nodeName). Written by the operator
	// only: the admission policy lets the lab checker of a node write the results of a lab only when its own node is listed here.
	// +optional
	DeviceNodes []string        `json:"deviceNodes,omitempty"`
	Connections []ConnectionRef `json:"connections,omitempty"`
	Access      []AccessEntry   `json:"access,omitempty"`
	// Scheduling is the place of the lab in the scheduler queue.
	// +optional
	Scheduling *SchedulingStatus `json:"scheduling,omitempty"`
	// ImageCache records whether this lab pulls its images through the platform
	// image cache, decided once on the first reconcile.
	// +optional
	ImageCache *bool `json:"imageCache,omitempty"`
	// ImageDigests are the digests the image tags of the lab's container devices
	// (and the netconfig image) were pinned to when the lab was created with the
	// image cache on, keyed by the image as written in the spec.
	// +optional
	ImageDigests map[string]string `json:"imageDigests,omitempty"`
	// ImageWarning lists the images that could not be pinned and are pulled by
	// their tag; empty when all were.
	// +optional
	ImageWarning string `json:"imageWarning,omitempty"`
	// Conditions surfaces reconciler progress/blocking reasons
	// (e.g. Ready=False reason=WaitingForInterface) for kubectl and clients.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// Checks are the results of the lab checks, one entry per check that has a result; the checker writes only changes.
	// +optional
	// +listType=map
	// +listMapKey=id
	Checks []CheckResult `json:"checks,omitempty"`
	// ChecksRunAck is the RunChecks request token the checker has taken (see the annotation names.AnnotationRunChecks).
	// +optional
	ChecksRunAck int64 `json:"checksRunAck,omitempty"`
}

// DeviceRef summarises a materialised Device's readiness.
type DeviceRef struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready,omitempty"`
	// State summarises the snapshots of a device with state persistence.
	// +optional
	State *DeviceStateInfo `json:"state,omitempty"`
	// Failure is the warning of a device pod that did not start in time.
	// +optional
	Failure *PodFailure `json:"failure,omitempty"`
	// CPUThrottle is the device's CPU throttling (see DeviceStatus.CPUThrottle); absent means none.
	// +optional
	CPUThrottle *DeviceCPUThrottle `json:"cpuThrottle,omitempty"`
	// Bootstrap is the bootstrap state of the device (see DeviceStatus.Bootstrap); absent without a bootstrap.
	// +optional
	Bootstrap *DeviceBootstrapStatus `json:"bootstrap,omitempty"`
}

// DeviceStateInfo is the organizer-facing view of a device's snapshots.
type DeviceStateInfo struct {
	LastSnapshotAt *metav1.Time `json:"lastSnapshotAt,omitempty"`
	RestoredAt     *metav1.Time `json:"restoredAt,omitempty"`
	SizeBytes      int64        `json:"sizeBytes,omitempty"`
	// QuotaWarning is set while snapshots are refused or failing.
	QuotaWarning string `json:"quotaWarning,omitempty"`
	// Rescue is true while the device runs in rescue mode.
	Rescue bool `json:"rescue,omitempty"`
}

// ConnectionRef summarises a materialised Connection's readiness.
type ConnectionRef struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready,omitempty"`
}

// AccessEntry describes a single externally reachable endpoint.
type AccessEntry struct {
	Device string `json:"device"`
	Port   int32  `json:"port"`
	// Protocol is http or https for a web port, and "terminal" for the browser terminal of the device.
	Protocol string `json:"protocol"`
	// URL is the address of a web port, and of the console of a terminal entry (https://<device>-<code>t.<labs domain>; the last character of the first label is the kind of access). It is empty
	// until the device has its code.
	URL string `json:"url,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Lab is the Schema for the labs API.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || !has(oldSelf.spec.lifecycle) || (has(self.spec) && has(self.spec.lifecycle))",message="explicit lifecycle intent cannot be removed with its spec"
type Lab struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LabSpec   `json:"spec,omitempty"`
	Status LabStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LabList contains a list of Lab.
type LabList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Lab `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lab{}, &LabList{})
}
