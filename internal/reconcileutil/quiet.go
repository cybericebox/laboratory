// Package reconcileutil holds what every controller of the operator and the node-agent shares.
package reconcileutil

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
		case ignoreNotFound && apierrors.IsNotFound(err):
			return reconcile.Result{}, nil
		}
		return res, err
	})
}
