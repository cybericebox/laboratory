//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"time"
)

// Ping asks OVS for a sign of life over the OpenFlow channel (the port description is the one request that needs an answer).
func (f *FlowManager) Ping() error { return f.client.RefreshPorts() }

// Watchdog calls ping every interval and calls onLost after failures misses in a row. The node-agent's two channels to OVS (the
// database and OpenFlow) do not reconnect: after an OVS crash they stay dead and every lab of the node stays dark until the pod
// restarts, so the node-agent exits (onLost) and the kubelet starts it again, which programs the bridge from scratch. It returns when
// ctx ends or after onLost.
func Watchdog(ctx context.Context, interval time.Duration, failures int, ping func(context.Context) error, onLost func(error)) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if failures <= 0 {
		failures = 3
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, interval/2+time.Second)
		err := ping(pctx)
		cancel()
		if err == nil {
			missed = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if missed++; missed >= failures {
			onLost(fmt.Errorf("OVS did not answer %d times in a row: %w", missed, err))
			return
		}
	}
}

// recreationGuard bounds how often the node-agent re-creates the same veth: a device with NET_ADMIN can delete its own interface in a
// loop, and each round would delete the port and its flows under the shared OVSDB lock and start over. At most max in window per key;
// past that the reconcile waits (the time returned) instead of recreating.
type recreationGuard struct {
	max    int
	window time.Duration
	hist   map[string][]time.Time
}

func newRecreationGuard(max int, window time.Duration) *recreationGuard {
	return &recreationGuard{max: max, window: window, hist: map[string][]time.Time{}}
}

// allow records a recreation and says whether it may go on; when not, how long to wait.
func (g *recreationGuard) allow(key string, now time.Time) (bool, time.Duration) {
	kept := g.hist[key][:0]
	for _, at := range g.hist[key] {
		if now.Sub(at) < g.window {
			kept = append(kept, at)
		}
	}
	if len(kept) >= g.max {
		g.hist[key] = kept
		return false, g.window - now.Sub(kept[0])
	}
	g.hist[key] = append(kept, now)
	return true, 0
}
