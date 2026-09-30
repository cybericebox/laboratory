package client

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// ErrorDomain is the errdetails.ErrorInfo domain of agent-specific errors.
	ErrorDomain = "agent.cybericebox"
	// ReasonTerminating is the ErrorInfo reason of a write refused because the
	// object with that deterministic name is still being deleted (finalizers run
	// asynchronously). The write was NOT applied; retry after a short delay.
	ReasonTerminating = "TERMINATING"
)

// IsTerminating reports whether err is the agent's retryable "object is still
// being deleted" refusal (codes.Unavailable + ErrorInfo reason TERMINATING).
// The object's deletion is finished once Get* returns codes.NotFound.
func IsTerminating(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.Domain == ErrorDomain && info.Reason == ReasonTerminating {
			return true
		}
	}
	return false
}
