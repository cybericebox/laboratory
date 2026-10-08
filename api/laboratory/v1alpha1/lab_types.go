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
	VPN       LabNetworkSpec    `json:"vpn,omitempty"`
	Internet  LabNetworkSpec    `json:"internet,omitempty"`
	// Devices of the lab, switches and hubs included. The ceiling is fixed in code (names.MaxLabDevices).
	// +kubebuilder:validation:MaxItems=64
	Devices     []DeviceTemplate     `json:"devices,omitempty"`
	Connections []ConnectionTemplate `json:"connections,omitempty"`
}

// LabNetworkSpec configures a network segment (VPN or internet) attached to the lab.
type LabNetworkSpec struct {
	Enabled    bool        `json:"enabled,omitempty"`
	DHCPServer *DHCPServer `json:"dhcpServer,omitempty"`
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
	// SecurityPreset names a device security profile (standard or extended; the old names basic, service,
	// net and debug are aliases). Only the name is exposed here; the concrete Linux capabilities behind it
	// are an internal platform decision. Empty means standard.
	SecurityPreset SecurityPreset `json:"securityPreset,omitempty"`
	// Interfaces of the device: at most 16 on a container (names.MaxContainerInterfaces), 48 on a switch or hub.
	// +kubebuilder:validation:MaxItems=48
	Interfaces []InterfaceSpec `json:"interfaces,omitempty"`
	Exposure   *ExposureSpec   `json:"exposure,omitempty"`
	// Resources sets the container resource requests/limits for this device.
	// +optional
	Resources *DeviceResources `json:"resources,omitempty"`
	// Persistence is the optional state-persistence policy of this device, set at
	// creation and immutable. The excluded paths and the quota are platform settings.
	// +optional
	Persistence *DevicePersistence `json:"persistence,omitempty"`
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
	Resources   *RuntimeAllocation `json:"resources,omitempty"`
	Phase       Phase              `json:"phase,omitempty"`
	VPN         LabNetworkStatus   `json:"vpn,omitempty"`
	Internet    LabNetworkStatus   `json:"internet,omitempty"`
	Devices     []DeviceRef        `json:"devices,omitempty"`
	Connections []ConnectionRef    `json:"connections,omitempty"`
	Access      []AccessEntry      `json:"access,omitempty"`
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
}

// LabNetworkStatus reports the allocated CIDR for a network segment.
type LabNetworkStatus struct {
	Ready bool   `json:"ready,omitempty"`
	CIDR  string `json:"cidr,omitempty"`
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
	Device   string `json:"device"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
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
