package grpc

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/client"
)

// Object names are deterministic (event team group, lab per challenge, client
// per participant), and deletes are asynchronous (finalizers). A create or
// update that lands on an object with DeletionTimestamp set would be applied to
// the dying object and vanish with it. So every write checks for that and
// refuses with a retryable codes.Unavailable / reason TERMINATING error; the
// caller retries until the object is gone (Get* returns NotFound) and the write
// then creates a fresh one.

// terminatingError is the retryable refusal for a write onto a terminating object.
func terminatingError(kind, name string) error {
	st := status.New(codes.Unavailable, fmt.Sprintf("%s %s is still being deleted, retry later", kind, name))
	if withDetails, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   client.ReasonTerminating,
		Domain:   client.ErrorDomain,
		Metadata: map[string]string{"kind": kind, "name": name},
	}); err == nil {
		st = withDetails
	}
	return st.Err()
}

// rejectTerminating returns the terminating error when obj is being deleted.
func rejectTerminating(kind string, obj metav1.Object) error {
	if obj.GetDeletionTimestamp() != nil {
		return terminatingError(kind, obj.GetName())
	}
	return nil
}

// createErr maps the error of a Create call. AlreadyExists means either a live
// object (idempotent create: reported as before) or a terminating one, which
// existing() re-reads to tell apart. A create into a namespace that is being
// deleted is terminating as well.
func createErr(err error, kind, name string, existing func() (metav1.Object, error)) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsAlreadyExists(err):
		if obj, getErr := existing(); getErr == nil {
			if terminating := rejectTerminating(kind, obj); terminating != nil {
				return terminating
			}
		}
	case apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause):
		return terminatingError(kind, name)
	}
	return err
}

// apiErrorInterceptor converts Kubernetes API errors returned by handlers into
// gRPC status codes (NotFound, AlreadyExists, ...) so callers can branch on
// status.Code. Errors that already are gRPC statuses pass through untouched.
func apiErrorInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	resp, err := h(ctx, req)
	return resp, apiErrorToStatus(err)
}

func apiErrorToStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var apiErr apierrors.APIStatus
	if !errors.As(err, &apiErr) {
		return err
	}
	code := codes.Unknown
	switch {
	case apierrors.IsNotFound(err):
		code = codes.NotFound
	case apierrors.IsAlreadyExists(err):
		code = codes.AlreadyExists
	case apierrors.IsConflict(err):
		code = codes.Aborted
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		code = codes.InvalidArgument
	case apierrors.IsForbidden(err):
		code = codes.PermissionDenied
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err):
		code = codes.Unavailable
	}
	return status.Error(code, err.Error())
}
