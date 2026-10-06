//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// R-14: the node-agent gives up (to be restarted) after the channel to OVS has been dead for several checks in a row, and a
// recovery resets the count.
func TestWatchdogExitsAfterConsecutiveMisses(t *testing.T) {
	var calls atomic.Int32
	lost := make(chan error, 1)
	go Watchdog(context.Background(), 10*time.Millisecond, 3, func(context.Context) error {
		calls.Add(1)
		return errors.New("dead")
	}, func(err error) { lost <- err })
	select {
	case err := <-lost:
		if calls.Load() != 3 || err == nil {
			t.Fatalf("calls %d: %v", calls.Load(), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watchdog must give up")
	}
}

func TestWatchdogForgivesASingleMiss(t *testing.T) {
	var n atomic.Int32
	lost := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watchdog(ctx, 5*time.Millisecond, 2, func(context.Context) error {
			// every other check fails: never two in a row
			if n.Add(1)%2 == 0 {
				return errors.New("blip")
			}
			return nil
		}, func(err error) { lost <- err })
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	select {
	case err := <-lost:
		t.Fatalf("one miss at a time must not be fatal: %v", err)
	default:
	}
}

func TestRecreationGuardCapsRecreationsPerPod(t *testing.T) {
	g := newRecreationGuard(3, time.Minute)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if ok, _ := g.allow("pod1/p123", now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("recreation %d is within the cap", i)
		}
	}
	ok, wait := g.allow("pod1/p123", now.Add(10*time.Second))
	if ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("the fourth in a minute must wait: %v %s", ok, wait)
	}
	if ok, _ := g.allow("pod2/p456", now.Add(10*time.Second)); !ok {
		t.Fatal("another pod is not held back by this one")
	}
	if ok, _ := g.allow("pod1/p123", now.Add(2*time.Minute)); !ok {
		t.Fatal("after the window it may go on")
	}
}
