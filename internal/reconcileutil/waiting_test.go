package reconcileutil

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A state to wait out is requeued, not reported as a reconciler error; a real failure keeps its error.
func TestWaitingIsRequeuedNotAnError(t *testing.T) {
	base := errors.New("port not there yet")
	wrapped := Quiet(reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, fmt.Errorf("resolve: %w", Waiting(base, 2*time.Second))
	}))
	res, err := wrapped.Reconcile(context.Background(), reconcile.Request{})
	if err != nil || res.RequeueAfter != 2*time.Second {
		t.Fatalf("waiting: %v %v", res, err)
	}
	real := Quiet(reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, base
	}))
	if _, err := real.Reconcile(context.Background(), reconcile.Request{}); !errors.Is(err, base) {
		t.Fatalf("a failure must stay an error: %v", err)
	}
	if !errors.Is(Waiting(base, 0), base) || !IsWaiting(Waiting(base, 0)) || Waiting(nil, 0) != nil {
		t.Fatal("Waiting must wrap, mark and pass nil")
	}
	var w waiting
	if !errors.As(Waiting(base, 0), &w) || w.retry != DefaultWaitRetry {
		t.Fatal("default retry")
	}
}
