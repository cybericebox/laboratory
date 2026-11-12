package checker

import (
	"context"
	"errors"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/probe"
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

// Prober is the third ability, of an Observer that can run the built-in observations: it asks the in-device program one question
// (never an expected value) and gets facts back. An Observer without it cannot judge directory, process, port, kernel_param, request
// or permission, and a check with one gets the result error.
type Prober interface {
	Probe(ctx context.Context, req probe.ObserveRequest) (probe.ObserveResponse, error)
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
		case builtinKind(p) != "":
			kinds[i] = builtinKind(p)
			verdicts[i] = observeBuiltin(ctx, p, obs, max(time.Until(deadline), time.Second))
		default:
			kinds[i] = "empty"
			verdicts[i] = partVerdict{Failed, []string{"the observation is empty"}}
		}
	}
	return combine(verdicts, kinds)
}

// FileProber is an Observer that can read a file through the in-device program. A file expectation of type or checksum always goes that
// way, on every device: the node-side reader follows links and so cannot say what the path itself is; one behaviour for all.
type FileProber interface {
	ProbeFile(ctx context.Context, path string, wantContent bool) (FileFacts, error)
}

func observeFile(ctx context.Context, p *labchecks.FilePart, obs Observer) partVerdict {
	wantContent := p.ContentContains != nil || p.ContentMatches != nil || p.Checksum != nil
	read := obs.ReadFile
	if p.Type != nil || p.Checksum != nil {
		fp, ok := obs.(FileProber)
		if !ok {
			return partVerdict{Failed, []string{"this device cannot read a file through the in-device program"}}
		}
		read = fp.ProbeFile
	}
	facts, err := read(ctx, p.Path, wantContent)
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

// builtinKind names the built-in observation of a part, or "".
func builtinKind(p *labchecks.Part) string {
	switch {
	case p.Directory != nil:
		return probe.KindDirectory
	case p.Process != nil:
		return probe.KindProcess
	case p.Port != nil:
		return probe.KindPort
	case p.KernelParam != nil:
		return probe.KindKernelParam
	case p.Request != nil:
		return probe.KindRequest
	case p.Permission != nil:
		return probe.KindPermission
	}
	return ""
}

// questionFor is what the in-device program is asked for a part: where to look, never what is expected.
func questionFor(p *labchecks.Part, left time.Duration) probe.ObserveRequest {
	switch {
	case p.Directory != nil:
		// The names are asked by lstat, one by one: the answer does not depend on the size of the directory.
		names := append(append([]string(nil), p.Directory.HasEntries...), p.Directory.LacksEntries...)
		return probe.ObserveRequest{Kind: probe.KindDirectory, Path: p.Directory.Path, Names: names}
	case p.Process != nil:
		// The scan of the whole process table is done by the program, which returns only whether one matched.
		q := probe.ObserveRequest{Kind: probe.KindProcess}
		if p.Process.Name != nil {
			q.Name = *p.Process.Name
		}
		if p.Process.Matches != nil {
			q.Matches = *p.Process.Matches
		}
		if p.Process.User != nil {
			q.User = *p.Process.User
		}
		return q
	case p.Port != nil:
		return probe.ObserveRequest{Kind: probe.KindPort, Port: int(p.Port.Port), Protocol: p.Port.Protocol}
	case p.KernelParam != nil:
		return probe.ObserveRequest{Kind: probe.KindKernelParam, Name: p.KernelParam.Name}
	case p.Request != nil:
		r := p.Request
		return probe.ObserveRequest{Kind: probe.KindRequest, Protocol: r.Protocol, Host: r.Host, Port: int(r.Port), Method: r.Method, URLPath: r.Path, TimeoutMs: left.Milliseconds()}
	}
	return probe.ObserveRequest{Kind: probe.KindPermission, User: p.Permission.User, Path: p.Permission.Path}
}

func observeBuiltin(ctx context.Context, p *labchecks.Part, obs Observer, left time.Duration) partVerdict {
	prober, ok := obs.(Prober)
	if !ok {
		return partVerdict{Failed, []string{"this device cannot run built-in observations"}}
	}
	resp, err := prober.Probe(ctx, questionFor(p, left))
	if err != nil {
		return partVerdict{Failed, []string{reasonOf(ctx, err)}}
	}
	if resp.Error != "" {
		return partVerdict{Failed, []string{"the device could not answer: " + resp.Error}}
	}
	switch {
	case p.Directory != nil && resp.Directory != nil:
		return evalDirectory(p.Directory, resp.Directory)
	case p.Process != nil && resp.Processes != nil:
		return evalProcess(p.Process, resp.Processes)
	case p.Port != nil && resp.Port != nil:
		return evalPort(p.Port, resp.Port)
	case p.KernelParam != nil && resp.KernelParam != nil:
		return evalKernelParam(p.KernelParam, resp.KernelParam)
	case p.Request != nil && resp.Request != nil:
		return evalRequest(p.Request, resp.Request)
	case p.Permission != nil && resp.Permission != nil:
		return evalPermission(p.Permission, resp.Permission)
	}
	return partVerdict{Failed, []string{"the device gave no answer of that kind"}}
}
