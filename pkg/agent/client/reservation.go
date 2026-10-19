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
