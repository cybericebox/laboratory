package flowacct

import (
	"context"
	"fmt"
	"time"
)

type PairCounters struct {
	Key
	ClientCIDR                               string // ephemeral trusted endpoint; never persisted or relayed
	BindingID, Epoch                         string
	PacketsOut, PacketsIn, BytesOut, BytesIn uint64
	Attempts, LabInitiatedAttempts           uint64
}

type CounterSnapshot struct {
	Rows    []PairCounters
	At      time.Time
	Partial bool
}

type CounterReader interface {
	ReadPairCounters(context.Context) (CounterSnapshot, error)
}

// PairCounterDelta never moves traffic to another identity. A raw counter
// reset yields the new value and marks a discontinuity instead of wrapping.
func PairCounterDelta(current, previous PairCounters) (PairCounters, bool, error) {
	if current.Subject == "" || current.Lab == "" || current.BindingID == "" || current.Epoch == "" {
		return PairCounters{}, true, fmt.Errorf("incomplete kernel counter identity")
	}
	if previous.Subject == "" && previous.Epoch == "" {
		return current, false, nil
	}
	if previous.Key != current.Key {
		return PairCounters{}, true, fmt.Errorf("kernel counter owner changed")
	}
	if previous.BindingID != current.BindingID || previous.Epoch != current.Epoch {
		return current, true, nil
	}
	partial := false
	difference := func(next, old uint64) uint64 {
		if next < old {
			partial = true
			return next
		}
		return next - old
	}
	d := current
	d.PacketsOut = difference(current.PacketsOut, previous.PacketsOut)
	d.PacketsIn = difference(current.PacketsIn, previous.PacketsIn)
	d.BytesOut = difference(current.BytesOut, previous.BytesOut)
	d.BytesIn = difference(current.BytesIn, previous.BytesIn)
	d.Attempts = difference(current.Attempts, previous.Attempts)
	d.LabInitiatedAttempts = difference(current.LabInitiatedAttempts, previous.LabInitiatedAttempts)
	return d, partial, nil
}
