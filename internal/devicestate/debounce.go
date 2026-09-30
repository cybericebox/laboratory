package devicestate

import (
	"context"
	"time"
)

// Debounce turns a stream of change signals into snapshot triggers: one trigger
// after the signals stayed quiet for quiet, or after maxWait of continuous
// signals (so a constantly written layer is still snapshotted). It returns when
// ctx ends or in is closed.
func Debounce(ctx context.Context, in <-chan struct{}, quiet, maxWait time.Duration, fire func()) {
	var (
		timer    *time.Timer
		deadline *time.Timer
		timerC   <-chan time.Time
		maxC     <-chan time.Time
	)
	stop := func(t *time.Timer) {
		if t != nil && !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}
	trigger := func() {
		stop(timer)
		stop(deadline)
		timer, deadline, timerC, maxC = nil, nil, nil, nil
		fire()
	}
	for {
		select {
		case <-ctx.Done():
			stop(timer)
			stop(deadline)
			return
		case _, ok := <-in:
			if !ok {
				stop(timer)
				stop(deadline)
				return
			}
			if timer == nil {
				timer = time.NewTimer(quiet)
				deadline = time.NewTimer(maxWait)
				maxC = deadline.C
			} else {
				stop(timer)
				timer.Reset(quiet)
			}
			timerC = timer.C
		case <-timerC:
			trigger()
		case <-maxC:
			trigger()
		}
	}
}
