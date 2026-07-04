// Package status provides shared helpers for setting Kubernetes-style status
// Conditions and emitting Events, so reconciler wait/failure states are visible
// via `kubectl describe` instead of being silent forever-requeues.
package status

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the single high-level condition every reconciled object
// carries: True when the object reached its desired state, False (with a reason)
// while blocked or failed.
const ConditionReady = "Ready"

// Common reasons — kept short and CamelCase per Kubernetes convention.
const (
	ReasonReady               = "Ready"
	ReasonProvisioning        = "Provisioning"
	ReasonValidationFailed    = "ValidationFailed"
	ReasonWaitingForInterface = "WaitingForInterface"
	ReasonWaitingForPort      = "WaitingForPort"
	ReasonWaitingForVPNServer = "WaitingForVPNServer"
	ReasonPubKeyCollision     = "PubKeyCollision"
	ReasonProgrammingFailed   = "ProgrammingFailed"
	ReasonPeerRegistered      = "PeerRegistered"
)

// SetReady sets (or updates) the Ready condition on conds. generation is the
// object's metadata.generation so stale conditions are detectable.
func SetReady(conds *[]metav1.Condition, generation int64, ready bool, reason, message string) {
	st := metav1.ConditionFalse
	if ready {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             st,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})
}

// IsReady reports whether the Ready condition is currently True.
func IsReady(conds []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conds, ConditionReady)
}
