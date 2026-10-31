package client

// Reasons of a FAILED ItemResult (ItemResult.reason) for the reservation rules. Every other failure has an empty reason
// and only the error text.
const (
	// ReasonReservationRequired: a LabGroup or Lab without reservation_id.
	ReasonReservationRequired = "RESERVATION_REQUIRED"
	// ReasonReservationNotFound: no reservation with that id in the caller's tenant.
	ReasonReservationNotFound = "RESERVATION_NOT_FOUND"
	// ReasonReservationEnded: the reservation takes no more work (ended, being deleted, past its end, or its tenant is
	// gone).
	ReasonReservationEnded = "RESERVATION_ENDED"
	// ReasonReservationFull: the item does not fit the rest of the reservation ("not enough room in this reservation").
	// Retryable: room may come back.
	ReasonReservationFull = "RESERVATION_FULL"
	// ReasonReservationMismatch: the item's deploy group already runs under another reservation, or an object that has a
	// reservation would be moved to another one.
	ReasonReservationMismatch = "RESERVATION_MISMATCH"
	// ReasonOverQuota: the tenant's overlapping reservations would peak above its quota.
	ReasonOverQuota = "OVER_QUOTA"
)

// Reasons of a FAILED Lab create item whose name is taken by a Lab of another generation.
const (
	// ReasonStaleLabReplacing: the name was taken by a Lab of an older generation (an older creation operation of the
	// same tenant and group); the agent has started deleting it. Retryable: repeat the same create until it succeeds.
	ReasonStaleLabReplacing = "STALE_LAB_REPLACING"
	// ReasonLabNameConflict: the name is taken by a Lab the agent will not replace on its own (a newer or an unrelated
	// generation, or no generation to compare). Not retryable; the caller must delete the Lab or choose another name.
	ReasonLabNameConflict = "LAB_NAME_CONFLICT"
)

// ReasonLabNotInGroup: an access policy names Labs that are not in the LabGroup (removed, never created, or of another
// group). Not retryable. The text lists every such lab id as `lab "id"` / `labs "id1", "id2"`; the caller drops them
// from the policy and sends it again.
const ReasonLabNotInGroup = "LAB_NOT_IN_GROUP"

// ReasonUnsupported: the agent does not advertise the capability the call needs (GetFeatures.lifecycle: per_lab_stop
// for StopLabs, retained_restart for StartLabs, full_group_stop for StopLabGroups and StartLabGroups). Not retryable;
// nothing was changed. The caller must check the capability before it dispatches.
const ReasonUnsupported = "UNSUPPORTED"

// Device profiles (devices[].profile of a lab spec; empty is container).
const (
	ProfileContainer = "container"
	ProfileMicroVM   = "microvm"
	// ProfileVM is reserved: the agent refuses it.
	ProfileVM = "vm"
)

// Reasons of a FAILED Lab create item whose devices ask for a profile that cannot run.
const (
	// ReasonProfileUnsupported: the profile is vm, unknown, or switched off in this laboratory. Not retryable.
	ReasonProfileUnsupported = "PROFILE_UNSUPPORTED"
	// ReasonProfileUnavailable: the profile is supported but no running node can run it, and no autoscaler node group is
	// known to add one (CapacityResponse.node_profiles: available_now false and not available_after_scale_up).
	// Not retryable now.
	ReasonProfileUnavailable = "PROFILE_UNAVAILABLE"
)

// ReasonPersistenceUnavailable: the lab asks for state persistence (devices[].persistence.enabled) and the lab
// nodes do not confirm that they can keep it (GetFeatures: state_persistence.available false, with an
// unavailable_reason). Nothing is created or started; the text carries only the fixed reason (`<CODE>:<n>/<m>`).
// Not retryable until the cluster is repaired.
const ReasonPersistenceUnavailable = "PERSISTENCE_UNAVAILABLE"
