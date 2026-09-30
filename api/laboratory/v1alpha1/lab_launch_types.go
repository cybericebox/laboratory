package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reasons a queued Lab waits, reported in LabLaunchStatus.Reason.
const (
	// LaunchReasonInFlightLimit: the maximum number of labs is already provisioning.
	LaunchReasonInFlightLimit = "InFlightLimit"
	// LaunchReasonPreparingImages: the images of the lab's class are being pulled onto the nodes.
	LaunchReasonPreparingImages = "PreparingImages"
	// LaunchReasonInsufficientResources: the cluster lacks free CPU or memory for the lab.
	LaunchReasonInsufficientResources = "InsufficientResources"
	// LaunchReasonNoSchedulableNodes: there is no node the lab pods could run on.
	LaunchReasonNoSchedulableNodes = "NoSchedulableNodes"
)

// LabLaunchStatus is the launch pacing state of a Lab. A new Lab is created at
// once but stays Queued until the operator's launcher admits it.
type LabLaunchStatus struct {
	// Class is the resolved launch class: Spec.LaunchClass, or a hash of the lab
	// topology and images when that is empty.
	Class string `json:"class,omitempty"`
	// AdmittedAt is when the launcher admitted the lab for provisioning.
	AdmittedAt *metav1.Time `json:"admittedAt,omitempty"`
	// Position is the 1-based place in the queue; 0 when the lab is not queued.
	// It is refreshed at a limited rate, so it can lag behind by a few seconds.
	Position int32 `json:"position,omitempty"`
	// Length is the number of queued labs when Position was written.
	Length int32 `json:"length,omitempty"`
	// Reason is why the queue is not admitting this lab right now (LaunchReason*).
	Reason string `json:"reason,omitempty"`
}
