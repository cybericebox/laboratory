// Package reconcileutil holds what every controller of the operator and the node-agent shares.
package reconcileutil

import (
	"context"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DefaultWaitRetry is how soon a reconcile that waits for something that is not there yet runs again.
const DefaultWaitRetry = 5 * time.Second

type waiting struct {
	err   error
	retry time.Duration
}

func (w waiting) Error() string { return w.err.Error() }
func (w waiting) Unwrap() error { return w.err }

// Waiting marks err as a state to wait out, not a failure: something the reconcile needs does not exist yet (a node still
// registering, a port not created yet). Quiet and QuietIgnoreNotFound log it as waiting at debug level and retry after retry
// (DefaultWaitRetry when not positive) instead of reporting a "Reconciler error" with back-off.
func Waiting(err error, retry time.Duration) error {
	if err == nil {
		return nil
	}
	if retry <= 0 {
		retry = DefaultWaitRetry
	}
	return waiting{err: err, retry: retry}
}

// IsWaiting says whether err (or what it wraps) is a Waiting state.
func IsWaiting(err error) bool {
	var w waiting
	return errors.As(err, &w)
}

// ConflictRetryDelay is how soon a reconcile that lost an optimistic-concurrency race runs again.
const ConflictRetryDelay = 200 * time.Millisecond

// Quiet wraps a reconciler so that a conflict (the object was modified; our copy was stale) is not reported as
// "Reconciler error": it is retried shortly with a fresh read. Every other error is returned unchanged, so real failures
// keep their error log and backoff.
func Quiet(r reconcile.Reconciler) reconcile.Reconciler { return wrap(r, false) }

// QuietIgnoreNotFound is Quiet that also treats NotFound (the object, or the one it works on, was deleted during the
// reconcile) as "nothing to do". For controllers whose objects are only ever deleted, never waited for.
func QuietIgnoreNotFound(r reconcile.Reconciler) reconcile.Reconciler { return wrap(r, true) }

func wrap(r reconcile.Reconciler, ignoreNotFound bool) reconcile.Reconciler {
	return reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		res, err := r.Reconcile(ctx, req)
		switch {
		case err == nil:
			return res, nil
		case apierrors.IsConflict(err):
			return reconcile.Result{RequeueAfter: ConflictRetryDelay}, nil
		case IsWaiting(err):
			var w waiting
			errors.As(err, &w)
			ctrl.LoggerFrom(ctx).V(1).Info("waiting", "reason", err.Error(), "retryAfter", w.retry.String())
			return reconcile.Result{RequeueAfter: w.retry}, nil
		case ignoreNotFound && apierrors.IsNotFound(err):
			return reconcile.Result{}, nil
		}
		return res, err
	})
}
