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
	// ReasonStaleLabReplacing: the name was taken by a Lab of an older generation (an older creation operation of the same
	// tenant and group); the agent has started deleting it. Retryable: repeat the same create until it succeeds.
	ReasonStaleLabReplacing = "STALE_LAB_REPLACING"
	// ReasonLabNameConflict: the name is taken by a Lab the agent will not replace on its own (a newer or an unrelated
	// generation, or no generation to compare). Not retryable; the caller must delete the Lab or choose another name.
	ReasonLabNameConflict = "LAB_NAME_CONFLICT"
)

// ReasonLabNotInGroup: an access policy names Labs that are not in the LabGroup (removed, never created, or of another
// group). Not retryable. The text lists every such lab id as `lab "id"` / `labs "id1", "id2"`; the caller drops them from the
// policy and sends it again.
const ReasonLabNotInGroup = "LAB_NOT_IN_GROUP"
