package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The scheduler paces how many pods start at once. Its units are the top-level
// objects, LabGroup (the VPN and gateway pods) and Lab (one pod per container
// device), and its slots are pods. It reads two operator-internal markers on
// those objects, never user labels: the label laboratory.cybericebox.com/deploy-group
// (the group key) and the annotation laboratory.cybericebox.com/deploy-after
// (comma separated group keys that must be complete first).

// PodScheduleState is where a pod is on its way from the queue to Ready.
// +kubebuilder:validation:Enum=Queued;Starting;Started;Failed
type PodScheduleState string

const (
	// PodQueued: waits for the scheduler to dispatch it; no workload exists yet.
	PodQueued PodScheduleState = "Queued"
	// PodStarting: dispatched; the workload is created and not Ready yet. It holds a slot.
	PodStarting PodScheduleState = "Starting"
	// PodStarted: was Ready once. It holds no slot any more.
	PodStarted PodScheduleState = "Started"
	// PodFailed: did not start in time (see Failure). It holds no slot; the
	// workload is left to retry on its own and may still become Ready.
	PodFailed PodScheduleState = "Failed"
)

// Reasons of a pod failure (PodFailure.Reason) and of a queue wait (SchedulingStatus.Reason).
const (
	FailureImagePull       = "ImagePull"
	FailureCrashLoop       = "CrashLoop"
	FailureUnschedulable   = "Unschedulable"
	FailureStartupTimeout  = "StartupTimeout"
	FailureDoesNotFit      = "DoesNotFit"
	WaitInFlightLimit      = "InFlightLimit"
	WaitForGroup           = "WaitingForGroup"
	WaitForTurn            = "WaitingForTurn"
	WaitPreparingImages    = "PreparingImages"
	WaitInsufficient       = "InsufficientResources"
	WaitNoSchedulableNodes = "NoSchedulableNodes"
	// WaitTenantQuota: the tenant of the object has reached its CPU or memory quota.
	WaitTenantQuota = "TenantQuota"
)

// PodSchedule is the scheduling state of one pod: a Device, or the VPN or
// gateway pod of a LabGroup. The scheduler owns it.
type PodSchedule struct {
	State PodScheduleState `json:"state,omitempty"`
	// QueuedAt is when the pod entered the queue.
	QueuedAt *metav1.Time `json:"queuedAt,omitempty"`
	// DispatchedAt is when the scheduler let the pod start; the startup timeout counts from it.
	DispatchedAt *metav1.Time `json:"dispatchedAt,omitempty"`
	// StartedAt is when the pod was first seen Ready.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// Failure explains a Failed pod; the warning clears when the pod becomes Ready.
	Failure *PodFailure `json:"failure,omitempty"`
}

// PodFailure is the warning of a pod that did not start.
type PodFailure struct {
	// Reason is one of the Failure* constants.
	Reason string `json:"reason,omitempty"`
	// Message is the last error the node reported (for example the image pull error).
	Message      string       `json:"message,omitempty"`
	RestartCount int32        `json:"restartCount,omitempty"`
	At           *metav1.Time `json:"at,omitempty"`
}

// NamedPodSchedule is the PodSchedule of a LabGroup pod ("vpn" or "gateway").
type NamedPodSchedule struct {
	Name        string `json:"name"`
	PodSchedule `json:",inline"`
}

// SchedulingStatus is the place of a Lab or LabGroup in the scheduler queue.
// The scheduler refreshes it at a limited rate, so Position can lag by seconds.
type SchedulingStatus struct {
	// Group is the deploy group of the object; empty for an independent one.
	Group string `json:"group,omitempty"`
	// Position is the 1-based place among the objects that still have pods to
	// dispatch; 0 when all pods are dispatched.
	Position int32 `json:"position,omitempty"`
	// Length is the number of such objects when Position was written.
	Length int32 `json:"length,omitempty"`
	// Reason is why the next pod of the object is not dispatched (Wait* constants).
	Reason string `json:"reason,omitempty"`
	// Message adds detail, for example "waiting for group intro".
	Message string `json:"message,omitempty"`
	// Pods is the number of pods of the object and Pending how many of them are
	// not dispatched yet.
	Pods    int32 `json:"pods,omitempty"`
	Pending int32 `json:"pending,omitempty"`
}
