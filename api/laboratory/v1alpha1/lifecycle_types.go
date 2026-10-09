package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabLifecycleSpec is explicit revision-fenced intent. Its absence means Running.
// Acceptance of intent never proves capture, access revocation or runtime release.
// +kubebuilder:validation:XValidation:rule="self.desiredState != 'Stopped' || has(self.snapshotMode)",message="stopped intent requires an explicit snapshot policy"
// +kubebuilder:validation:XValidation:rule="!has(self.terminal) || !self.terminal || self.desiredState == 'Stopped'",message="terminal labs must stay stopped"
// +kubebuilder:validation:XValidation:rule="self.revision > oldSelf.revision || self == oldSelf",message="lifecycle revision must increase for a changed intent"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.terminal) || !oldSelf.terminal || (has(self.terminal) && self.terminal)",message="terminal intent cannot be cleared"
type LabLifecycleSpec struct {
	// +kubebuilder:validation:Enum=Running;Stopped
	DesiredState string `json:"desiredState"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	OperationID string `json:"operationId"`
	// +kubebuilder:validation:Minimum=1
	Revision int64 `json:"revision"`
	// +kubebuilder:validation:Enum=Skip;Required
	// +optional
	SnapshotMode string `json:"snapshotMode,omitempty"`
	// Terminal is monotonic: solved laboratories can never be restarted.
	// +optional
	Terminal bool `json:"terminal,omitempty"`
	// +optional
	RetentionUntil *metav1.Time `json:"retentionUntil,omitempty"`
}

func (s *LabLifecycleSpec) IsStopped() bool { return s != nil && s.DesiredState == "Stopped" }

// LabLifecycleStatus is an observation owned by the operator. All acknowledgements
// refer to the exact UID, operation, revision and observed object generation.
type LabLifecycleStatus struct {
	// +kubebuilder:validation:Enum=Running;Snapshotting;Stopping;Stopped;StopFailed;Starting;Unknown
	ObservedState string `json:"observedState"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	OperationID string `json:"operationId"`
	// +kubebuilder:validation:Minimum=1
	Revision           int64        `json:"revision"`
	LabUID             string       `json:"labUID"`
	ObservedGeneration int64        `json:"observedGeneration"`
	Reason             string       `json:"reason,omitempty"`
	Error              string       `json:"error,omitempty"`
	RequestedAt        *metav1.Time `json:"requestedAt,omitempty"`
	StoppedAt          *metav1.Time `json:"stoppedAt,omitempty"`
	SnapshotComplete   bool         `json:"snapshotComplete,omitempty"`
	// AccessFenced is current-operation confirmation, never RPC acceptance.
	AccessFenced         bool         `json:"accessFenced,omitempty"`
	AccessFencedAt       *metav1.Time `json:"accessFencedAt,omitempty"`
	AccessFenceVPNBootID string       `json:"accessFenceVPNBootId,omitempty"`
}

// ResourceAmounts separates configured, held and measured compute amounts.
type ResourceAmounts struct {
	// +kubebuilder:validation:Minimum=0
	CPUMillicores int64 `json:"cpuMillicores"`
	// +kubebuilder:validation:Minimum=0
	MemoryBytes int64 `json:"memoryBytes"`
}

// RuntimeAllocation keeps held requests until identity-matching native absence
// and attachment cleanup are confirmed. Missing observations mean Unknown.
// Quota bytes are uncompressed accounting, not physically reclaimed storage.
type RuntimeAllocation struct {
	ConfiguredRequests ResourceAmounts `json:"configuredRequests"`
	ConfiguredLimits   ResourceAmounts `json:"configuredLimits"`
	AllocatedRequests  ResourceAmounts `json:"allocatedRequests"`
	// +kubebuilder:validation:Enum=Allocated;Releasing;Released;Unknown
	RuntimeState   string           `json:"runtimeState"`
	ObservedAt     *metav1.Time     `json:"observedAt,omitempty"`
	ReleasedAt     *metav1.Time     `json:"releasedAt,omitempty"`
	UsageAvailable bool             `json:"usageAvailable,omitempty"`
	Used           *ResourceAmounts `json:"used,omitempty"`
	// +kubebuilder:validation:Minimum=0
	SnapshotQuotaBytes int64 `json:"snapshotQuotaBytes,omitempty"`
	// +kubebuilder:validation:Enum=None;Retained;DeleteRequested;CleanupPending;Deleted;Unknown
	StorageState                  string `json:"storageState"`
	PhysicalStorageBytesAvailable bool   `json:"physicalStorageBytesAvailable,omitempty"`
	// +kubebuilder:validation:Minimum=0
	PhysicalStorageBytes int64  `json:"physicalStorageBytes,omitempty"`
	OperationID          string `json:"operationId,omitempty"`
	Revision             int64  `json:"revision,omitempty"`
}

// DeviceCaptureRequest is an operator-owned required checkpoint request. Its
// deadline bounds precommit capture/holds; expiry is failure, never permission
// to delete. A durable committed hold outlives that deadline until native death
// or the approved API-fenced cancellation of its exact operation.
type DeviceCaptureRequest struct {
	// CommitNodeAgentEpoch requests a durable hold for this exact capture boot.
	// +optional
	CommitNodeAgentEpoch string `json:"commitNodeAgentEpoch,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	OperationID string `json:"operationId"`
	// +kubebuilder:validation:Minimum=1
	LifecycleRevision int64 `json:"lifecycleRevision"`
	// +kubebuilder:validation:MinLength=1
	PodUID string `json:"podUID"`
	// PodResourceVersion is capture-time audit data; deletion reads a fresh RV.
	// +kubebuilder:validation:MinLength=1
	PodResourceVersion string `json:"podResourceVersion"`
	// +kubebuilder:validation:Minimum=0
	Epoch int32 `json:"epoch"`
	// +kubebuilder:validation:Minimum=0
	Incarnation int32 `json:"incarnation"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	DeadlineSeconds int32 `json:"deadlineSeconds"`
}

// DeviceCaptureResult is node-agent-owned, independent of ExitSnapshotPod.
// Succeeded includes unchanged base/latest state; a stale or invalidated held
// guard is never sufficient to stop. NodeAgentEpoch names the capture boot; a
// committed journal retains that identity through recovery until native death
// or API-fenced cancellation. Uncommitted prior-boot holds are invalidated.
type DeviceCaptureResult struct {
	// Committed is acknowledged only after the exact hold journal is fsynced.
	// It prevents deadline-only thaw between collective capture and deletion.
	// +optional
	Committed bool `json:"committed,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	OperationID string `json:"operationId"`
	// +kubebuilder:validation:Minimum=1
	LifecycleRevision int64 `json:"lifecycleRevision"`
	// +kubebuilder:validation:MinLength=1
	PodUID string `json:"podUID"`
	// +kubebuilder:validation:MinLength=1
	PodResourceVersion string `json:"podResourceVersion"`
	// +kubebuilder:validation:Minimum=0
	Epoch int32 `json:"epoch"`
	// +kubebuilder:validation:Minimum=0
	Incarnation int32 `json:"incarnation"`
	// +kubebuilder:validation:MinLength=1
	NodeAgentEpoch string `json:"nodeAgentEpoch"`
	// +kubebuilder:validation:Enum=Pending;Succeeded;Failed
	Result     string       `json:"result"`
	Image      string       `json:"image,omitempty"`
	SnapshotAt *metav1.Time `json:"snapshotAt,omitempty"`
	// +kubebuilder:validation:Minimum=0
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Error     string `json:"error,omitempty"`
	Quiesced  bool   `json:"quiesced,omitempty"`
	// +kubebuilder:validation:Enum=Held;Invalidated;Released;Unknown
	GuardState string       `json:"guardState"`
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}
