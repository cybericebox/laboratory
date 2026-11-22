package portkeeper

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct {
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

// The getty pattern: a program that ends at once is started again after 1 s, then 2, 4 ... at most 30 s; one that ran a while is started
// again after 1 s.
func TestSuperviseBacksOff(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	run := func() error {
		runs++
		switch runs {
		case 8:
			clock.now = clock.now.Add(time.Minute) // a login session that lasted
		case 10:
			cancel()
		}
		return errors.New("ended")
	}
	Supervise(ctx, clock, run)
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 1, 2}
	if len(clock.slept) != len(want) {
		t.Fatalf("slept %v", clock.slept)
	}
	for i, w := range want {
		if clock.slept[i] != w*time.Second {
			t.Fatalf("slept %v, want %v seconds", clock.slept, want)
		}
	}
}
