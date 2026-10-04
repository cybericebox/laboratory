package client

import (
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReasonCountMismatch is the ErrorInfo reason of a selector call refused because
// more objects matched than the call's expected_count. Nothing was done.
const ReasonCountMismatch = "COUNT_MISMATCH"

// CountMismatch reports whether err is the agent's expected_count refusal
// (codes.FailedPrecondition + reason COUNT_MISMATCH) and the actual number of
// objects the selector matched.
func CountMismatch(err error) (actual int, ok bool) {
	st, isStatus := status.FromError(err)
	if !isStatus || st.Code() != codes.FailedPrecondition {
		return 0, false
	}
	for _, d := range st.Details() {
		info, isInfo := d.(*errdetails.ErrorInfo)
		if isInfo && info.Domain == ErrorDomain && info.Reason == ReasonCountMismatch {
			n, convErr := strconv.Atoi(info.Metadata["actual"])
			return n, convErr == nil
		}
	}
	return 0, false
}
