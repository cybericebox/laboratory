package portkeeper

import (
	"context"
	"time"
)

// Respawn timing of a console program (the getty pattern): started again after FirstDelay, doubling up to MaxDelay while it ends at
// once; a program that ran at least QuickRun is started again after FirstDelay.
const (
	FirstDelay = time.Second
	MaxDelay   = 30 * time.Second
	QuickRun   = 10 * time.Second
)

// Clock is time for Supervise (a test drives it).
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Supervise runs run again and again until ctx ends, with the respawn delays. run returns when the program (or the connection) ended.
func Supervise(ctx context.Context, clock Clock, run func() error) {
	delay := FirstDelay
	for ctx.Err() == nil {
		start := clock.Now()
		_ = run()
		if ctx.Err() != nil {
			return
		}
		if clock.Now().Sub(start) >= QuickRun {
			delay = FirstDelay
		}
		if clock.Sleep(ctx, delay) != nil {
			return
		}
		delay = min(delay*2, MaxDelay)
	}
}
