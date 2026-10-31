package nodeagent

import (
	"context"
	"time"

	"github.com/go-logr/logr"
)

// PreflightRetry repeats the start-up check of state persistence while it fails. A check that fails at the start of a node-agent is often
// early, not wrong: the registry of a fresh cluster is not up yet, the container runtime is still starting. The node-agent starts without
// the snapshot engine (and says so on the Node); this brings the engine up in the running process as soon as the check passes, and keeps
// the Node annotation current while it fails. It runs only when persistence was asked for and the first check failed.
type PreflightRetry struct {
	Log logr.Logger
	// Input is the check; only the persistence part is repeated.
	Input PreflightInput
	// First is the annotation value the first check gave (unavailable: ...).
	First string
	// Enable starts the engine; Annotate puts the annotation on the node; Told tells the recovery (an Event). Told may be nil.
	Enable   func() error
	Annotate func(ctx context.Context, value string) error
	Told     func(message string)
	// Interval is the time between checks (30 s by default); Check is the check itself (Preflight by default).
	Interval time.Duration
	Check    func(ctx context.Context, in PreflightInput) PreflightResult
}

// NeedLeaderElection: every node-agent looks after its own node.
func (r *PreflightRetry) NeedLeaderElection() bool { return false }

// Start blocks until the engine is up (or ctx ends).
func (r *PreflightRetry) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	check := r.Check
	if check == nil {
		check = Preflight
	}
	in := r.Input
	in.Persistence, in.Observation = true, false
	last := r.First
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := check(ctx, in).StatePersistence
		if now == PreflightReady {
			if err := r.Enable(); err != nil {
				r.Log.Error(err, "the check passes but the snapshot engine cannot start; retrying")
				continue
			}
			if err := r.Annotate(ctx, PreflightReady); err != nil {
				r.Log.Error(err, "state persistence is on but the node annotation could not be updated")
			}
			r.Log.Info("state persistence is available on this node now: the snapshot engine started")
			if r.Told != nil {
				r.Told("state persistence became available on this node")
			}
			return nil
		}
		if now != last {
			last = now
			if err := r.Annotate(ctx, now); err != nil {
				r.Log.Error(err, "cannot update the node annotation of state persistence")
			}
		}
	}
}
