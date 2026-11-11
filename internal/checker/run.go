package checker

import (
	"context"
	"errors"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
)

// ExecSpec is a custom observation handed to an Executor.
type ExecSpec struct {
	Command     []string
	Script      string
	Interpreter string
	User        string
	// Limit is what is left of the check's time limit.
	Limit time.Duration
}

// Observer is the two abilities the design asks of the checker for a device (docs/specs/2026-11-11-lab-checkers-bootstrap-devices.md,
// section 5): read a file and run a command. The node-side implementation reads the files of a container from the node and execs
// through the runtime; the micro-VM one asks the in-device tool for both.
type Observer interface {
	ReadFile(ctx context.Context, path string, wantContent bool) (FileFacts, error)
	Exec(ctx context.Context, spec ExecSpec) (ExecResult, error)
}

// ObserveError is an error whose Reason is safe to publish in a result detail (it names no path of the node and no content).
type ObserveError struct {
	Reason string
	Err    error
}

func (e *ObserveError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *ObserveError) Unwrap() error { return e.Err }

// reasonOf maps an error of an Observer to the text that goes into a detail.
func reasonOf(ctx context.Context, err error) string {
	var oe *ObserveError
	switch {
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return "the check ran over its time limit"
	case errors.Is(err, ErrPseudoPath):
		return ErrPseudoPath.Error()
	case errors.Is(err, ErrTooManyLinks):
		return "too many symbolic links"
	case errors.As(err, &oe):
		return oe.Reason
	}
	return "the device could not be observed"
}

// RunCheck makes one run of a check: every part in order, all within the check's time limit, judged together. The observations
// are made one after another, so the parts "hold in the same run" of the checker, not at one instant of the device.
func RunCheck(ctx context.Context, c *labchecks.Check, obs Observer) Run {
	limit := time.Duration(c.TimeLimitSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	deadline, _ := ctx.Deadline()
	verdicts := make([]partVerdict, len(c.Parts))
	kinds := make([]string, len(c.Parts))
	for i := range c.Parts {
		p := &c.Parts[i]
		switch {
		case p.File != nil:
			kinds[i] = "file"
			verdicts[i] = observeFile(ctx, p.File, obs)
		case p.Custom != nil:
			kinds[i] = "custom"
			verdicts[i] = observeCustom(ctx, p.Custom, obs, max(time.Until(deadline), time.Second))
		default:
			kinds[i] = "empty"
			verdicts[i] = partVerdict{Failed, []string{"the observation is empty"}}
		}
	}
	return combine(verdicts, kinds)
}

func observeFile(ctx context.Context, p *labchecks.FilePart, obs Observer) partVerdict {
	wantContent := p.ContentContains != nil || p.ContentMatches != nil
	facts, err := obs.ReadFile(ctx, p.Path, wantContent)
	if err != nil {
		return partVerdict{Failed, []string{reasonOf(ctx, err)}}
	}
	return evalFile(p, facts)
}

func observeCustom(ctx context.Context, p *labchecks.CustomPart, obs Observer, left time.Duration) partVerdict {
	res, err := obs.Exec(ctx, ExecSpec{Command: p.Command, Script: p.Script, Interpreter: p.Interpreter, User: p.User, Limit: left})
	if err != nil {
		return partVerdict{Failed, []string{reasonOf(ctx, err)}}
	}
	return evalCustom(p, res)
}
