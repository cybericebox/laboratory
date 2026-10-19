package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReservationPhase is the lifecycle phase of a Reservation.
// +kubebuilder:validation:Enum=Pending;Active;Ending;Ended
type ReservationPhase string

const (
	// ReservationPhasePending: the window has not started yet.
	ReservationPhasePending ReservationPhase = "Pending"
	// ReservationPhaseActive: the window is running.
	ReservationPhaseActive ReservationPhase = "Active"
	// ReservationPhaseEnding: the ending notice has been given, the window is about to close.
	ReservationPhaseEnding ReservationPhase = "Ending"
	// ReservationPhaseEnded: the window is over and its groups are deleted.
	ReservationPhaseEnded ReservationPhase = "Ended"
)

// Condition types of a Reservation.
const (
	// ReservationConditionEnding is true from the ending notice until the window closes.
	ReservationConditionEnding = "Ending"
	// ReservationConditionShortfall is true when the cluster cannot give the reserved resources.
	ReservationConditionShortfall = "Shortfall"
	// ReservationConditionInvalid is true when spec.resources reserves nothing: a resource other than cpu and memory, or cpu
	// or memory missing, zero or negative. The phase still follows the clock.
	ReservationConditionInvalid = "Invalid"
	// ReservationConditionExceeded is true while the reservation was made smaller (a newer generation with a lower amount)
	// than what runs under it; still true one notice after it was set, the groups created before the notice are deleted and it
	// turns False with reason GroupsDeleted.
	ReservationConditionExceeded = "Exceeded"
	// ReservationConditionWithinAmount says whether status.used fits the amount; its message keeps the generation and amount
	// of the last fit ("last fit: generation=G cpu=Cm memory=M"), which tells a shrink from accounting drift.
	ReservationConditionWithinAmount = "WithinAmount"
	// ReservationConditionOverAmount is true while status.used is over the amount for a reason other than a shrink (the
	// scheduler recounted what runs: device defaults, edited labs, moved labels). Informational: nothing is deleted for it.
	ReservationConditionOverAmount = "OverAmount"
)

// Reasons of the Ending condition.
const (
	// ReservationReasonWindowClosing: the window closes at spec.to.
	ReservationReasonWindowClosing = "WindowClosing"
	// ReservationReasonTenantGone: the tenant of the reservation no longer exists, so the reservation ends one notice after
	// that was seen (or at spec.to when that is sooner), and stays ended.
	ReservationReasonTenantGone = "TenantGone"
	// ReservationReasonExtended: spec.to was moved past the notice, the end is called off.
	ReservationReasonExtended = "Extended"
	// ReservationReasonEnded: the window is closed and its groups are deleted.
	ReservationReasonEnded = "Ended"
	// ReservationReasonAmountReduced: Exceeded, the reservation was made smaller than what runs under it.
	ReservationReasonAmountReduced = "AmountReduced"
	// ReservationReasonGroupsDeleted: Exceeded is over, the groups were deleted for the shrink of this generation.
	ReservationReasonGroupsDeleted = "GroupsDeleted"
)

// ReservationSpec claims cpu and memory of the cluster for a tenant in a time window. The groups of
// the tenant that carry the reservation label run inside the claim.
// +kubebuilder:validation:XValidation:rule="self.to > self.from",message="spec.to must be after spec.from"
type ReservationSpec struct {
	// Tenant is the tenant the reservation belongs to (Tenant name, i.e. client certificate CN).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Tenant string `json:"tenant"`
	// From is when the reservation starts.
	// +kubebuilder:validation:Required
	From metav1.Time `json:"from"`
	// To is when the reservation ends and its groups are deleted.
	// +kubebuilder:validation:Required
	To metav1.Time `json:"to"`
	// Resources is what is reserved (cpu and memory).
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['cpu', 'memory'])",message="resources names only cpu and memory"
	// +optional
	Resources corev1.ResourceList `json:"resources,omitempty"`
}

// ReservationStatus is what the operator observed.
type ReservationStatus struct {
	// Phase is the lifecycle phase.
	// +optional
	Phase ReservationPhase `json:"phase,omitempty"`
	// Used is what the groups of the reservation request now.
	// +optional
	Used corev1.ResourceList `json:"used,omitempty"`
	// Groups is how many lab groups run under the reservation.
	// +optional
	Groups int32 `json:"groups,omitempty"`
	// NoticeAt is when the ending notice was given.
	// +optional
	NoticeAt *metav1.Time `json:"noticeAt,omitempty"`
	// Conditions are the Ending and Shortfall conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=res
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.from`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.spec.to`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="Reservation name is at most 63 characters (it is a label value)"

// Reservation is the Schema for the reservations API: cpu and memory claimed for a tenant in a
// time window.
type Reservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReservationSpec   `json:"spec,omitempty"`
	Status ReservationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReservationList contains a list of Reservation.
type ReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Reservation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Reservation{}, &ReservationList{})
}
