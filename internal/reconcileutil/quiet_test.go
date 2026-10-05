package reconcileutil

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func run(err error) (reconcile.Result, error) { return runWith(Quiet, err) }

func runWith(q func(reconcile.Reconciler) reconcile.Reconciler, err error) (reconcile.Result, error) {
	return q(reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, err
	})).Reconcile(context.Background(), reconcile.Request{})
}

func TestQuiet(t *testing.T) {
	gr := schema.GroupResource{Resource: "devices"}
	if res, err := run(apierrors.NewConflict(gr, "d", errors.New("the object has been modified"))); err != nil || res.RequeueAfter != ConflictRetryDelay {
		t.Fatalf("conflict: want quiet requeue, got %+v %v", res, err)
	}
	if res, err := runWith(QuietIgnoreNotFound, apierrors.NewNotFound(gr, "d")); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("not found: want nil, got %+v %v", res, err)
	}
	if _, err := run(apierrors.NewNotFound(gr, "d")); err == nil {
		t.Fatal("plain Quiet must keep NotFound")
	}
	if _, err := run(nil); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err := run(boom); !errors.Is(err, boom) {
		t.Fatalf("other errors must pass through, got %v", err)
	}
}
