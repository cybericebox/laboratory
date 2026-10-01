package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeviceSpec defines the desired state of Device.
type DeviceSpec struct {
	// LabRef is the name of the parent Lab.
	// +kubebuilder:validation:Required
	LabRef string `json:"labRef"`
	// Name is the logical device name within the lab (matches DeviceTemplate.name).
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Type  DeviceType `json:"type"`
	Image string     `json:"image,omitempty"`
	// SecurityPreset names a capability profile (basic/service/net/debug); the
	// concrete capabilities are resolved internally by the operator.
	SecurityPreset SecurityPreset  `json:"securityPreset,omitempty"`
	Interfaces     []InterfaceSpec `json:"interfaces,omitempty"`
	Exposure       *ExposureSpec   `json:"exposure,omitempty"`
	// Resources sets the container resource requests/limits for this device.
	// +optional
	Resources *DeviceResources `json:"resources,omitempty"`
	// State is the optional state-persistence policy and operator controls of
	// this device. Set once when the Device is materialised: a Lab created while
	// the platform has device state persistence off never carries it.
	// +optional
	State *DeviceStateSpec `json:"state,omitempty"`
	// ImageMirror is the cache prefix (host:port, e.g. "localhost:5035") through
	// which the node pulls the device image: the operator pulls REG/repo:tag as
	// <prefix>/REG/repo:tag. Set once when the Device is materialised, only for
	// labs created while the image cache is on; empty pulls Image directly.
	// +optional
	ImageMirror string `json:"imageMirror,omitempty"`
	// ImageDigests pins the images of the device (its image and the netconfig
	// init-container image) to the digests their tags had when the lab was
	// created, keyed by the image as written. A cached pull uses the digest, so
	// every device of the lab, and of the labs created in the same wave, runs the
	// same image even if an upstream tag moves. An image that could not be
	// resolved is absent and pulled by its tag.
	// +optional
	ImageDigests map[string]string `json:"imageDigests,omitempty"`
	// Code is the short random code that makes the workload name <name>-<code>
	// unique in the group namespace; the web Service of the device carries the same
	// one. It is drawn when the Device is created and never changes. Empty on a
	// Device that predates codes: its workload keeps the name of the Device.
	// +optional
	Code string `json:"code,omitempty"`
	// Env values are NOT carried on the CR — they live only in a per-device
	// Secret (<device>-env) the agent writes, referenced by the pod via envFrom.
}

// DeviceResources sets container resource requests/limits for a device pod.
// Values are Kubernetes quantity strings (e.g. "250m", "256Mi"); empty fields
// are omitted so the scheduler treats them as best-effort. Requests are what
// capacity planning sums against node allocatable.
type DeviceResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// StateEnabled reports whether the device is snapshot-backed.
func (s DeviceSpec) StateEnabled() bool { return s.State != nil && s.State.Enabled }

// DeviceStateSpec turns a device into a bare Pod whose writable layer is
// snapshotted by the node-agent into the platform snapshot registry, so an
// unplanned container restart does not lose the participant's work. The policy
// fields are copied from the operator configuration when the Device is created
// and never change afterwards; ResetToken and Rescue are operator controls.
type DeviceStateSpec struct {
	// Enabled marks the device as snapshot-backed. False or absent: the device
	// runs as a Deployment exactly as without the feature.
	Enabled bool `json:"enabled,omitempty"`
	// Debounce is how long the writable layer must stay quiet after a change
	// before the node-agent takes a snapshot.
	// +optional
	Debounce metav1.Duration `json:"debounce,omitempty"`
	// ExcludePaths are absolute paths inside the container that are never
	// snapshotted (temporary and runtime directories).
	// +optional
	ExcludePaths []string `json:"excludePaths,omitempty"`
	// WriteQuotaBytes is the write quota of this device: the most of the user's
	// writes kept (the uncompressed size of all snapshot layers). Over the quota
	// the last good snapshot is kept and a warning is reported.
	// +optional
	WriteQuotaBytes int64 `json:"writeQuotaBytes,omitempty"`
	// MaxFileBytes: a regular file larger than this is left out of the snapshots
	// (the status warning names it). Zero is the platform default.
	// +optional
	MaxFileBytes int64 `json:"maxFileBytes,omitempty"`
	// MaxLayers is the number of snapshot layers after which the chain is
	// squashed into one.
	// +optional
	MaxLayers int32 `json:"maxLayers,omitempty"`
	// ResetToken: a new value discards the snapshots and starts the device from
	// its base image again. Set by the management agent (ResetDevice).
	// +optional
	ResetToken string `json:"resetToken,omitempty"`
	// Rescue starts the device from its snapshot with a shell instead of the
	// image entrypoint, to repair a configuration that makes it crash. Set by
	// the management agent (RescueDevice).
	// +optional
	Rescue bool `json:"rescue,omitempty"`
}

// DeviceStateStatus is the observed state of a snapshot-backed device. The
// controller owns Epoch, Incarnation, StoppedAt, CrashStreak, RestoredAt,
// ResetToken and Rescue; the node-agent owns the snapshot fields and
// ExitSnapshotPod. Each side patches only its own fields.
type DeviceStateStatus struct {
	// Epoch counts resets. A pod carries the epoch it was created in; the
	// node-agent ignores snapshots of pods from an older epoch.
	Epoch int32 `json:"epoch,omitempty"`
	// Incarnation numbers the pods of this device (pod name suffix).
	Incarnation int32 `json:"incarnation,omitempty"`
	// Image is the latest snapshot image (registry reference with digest). Empty
	// before the first snapshot and after a reset: the pod starts from the base
	// image.
	Image string `json:"image,omitempty"`
	// SnapshotAt is when Image was taken.
	SnapshotAt *metav1.Time `json:"snapshotAt,omitempty"`
	// SizeBytes is the uncompressed size of all snapshot layers of Image.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Layers is the number of snapshot layers on top of the base image.
	Layers int32 `json:"layers,omitempty"`
	// Warning is set while the last snapshot attempt failed or was refused
	// (quota exceeded); the previous good snapshot stays in Image.
	Warning string `json:"warning,omitempty"`
	// ExitSnapshotPod is the pod whose exit snapshot is finished (taken or
	// given up). The controller recreates the pod only after this marker.
	ExitSnapshotPod string `json:"exitSnapshotPod,omitempty"`
	// StoppedAt is when the controller saw the current pod end.
	StoppedAt *metav1.Time `json:"stoppedAt,omitempty"`
	// CrashStreak counts consecutive pods that ended within a short time of
	// starting; it drives the recreation back-off.
	CrashStreak int32 `json:"crashStreak,omitempty"`
	// RestoredAt is when a pod was last created from a snapshot.
	RestoredAt *metav1.Time `json:"restoredAt,omitempty"`
	// ResetToken is the last ResetToken spec value acted on.
	ResetToken string `json:"resetToken,omitempty"`
	// Rescue reports whether the current pod runs in rescue mode.
	Rescue bool `json:"rescue,omitempty"`
}

// DeviceStatus defines the observed state of Device.
type DeviceStatus struct {
	Ready    bool   `json:"ready,omitempty"`
	NodeName string `json:"nodeName,omitempty"`
	// NodeAddress is the node IP used as Geneve VTEP address.
	NodeAddress string `json:"nodeAddress,omitempty"`
	PodIP       string `json:"podIP,omitempty"`
	// PodName is the current pod backing this device. A device runs one pod
	// (Deployment, replicas=1, or a bare Pod with state persistence) whose name
	// changes on recreation, so the
	// node-agent keys the device's per-pod OVS port on this stable pointer.
	PodName string `json:"podName,omitempty"`
	// VNI is set only for unmanaged-switch and hub device types.
	VNI    *uint  `json:"vni,omitempty"`
	Reason string `json:"reason,omitempty"`
	// State is the snapshot state of a device with spec.state.enabled.
	// +optional
	State *DeviceStateStatus `json:"state,omitempty"`
	// Scheduling is the pod's place on its way from the scheduler queue to Ready.
	// +optional
	Scheduling *PodSchedule `json:"scheduling,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Device is the Schema for the devices API.
type Device struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DeviceSpec   `json:"spec,omitempty"`
	Status DeviceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DeviceList contains a list of Device.
type DeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Device `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Device{}, &DeviceList{})
}
