package checker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cybericebox/laboratory/internal/probe"
)

// execObserver observes a device through the in-device tool (the static program every device carries at names.ProbeBinary), which the
// device access service runs in the device on the node's behalf. Every observation of every device goes this way: the node never reads a
// container's files.
type execObserver struct {
	runner Runner
	pod    PodInfo
}

// execGrace is added to the time limit of a command for the exec itself (the tool kills the command at the limit and answers).
const execGrace = 5 * time.Second

// ask runs one tool command and decodes its one JSON answer.
func (o *execObserver) ask(ctx context.Context, cmd string, req, resp any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	limit := execGrace
	if dl, ok := ctx.Deadline(); ok {
		limit = max(time.Until(dl), time.Second)
	}
	out, exit, err := o.runner.RunProbe(ctx, o.pod, cmd, string(raw), limit+execGrace)
	if err != nil {
		return err
	}
	if exit != 0 || json.Unmarshal(out, resp) != nil {
		// The tool is missing (exit 126 or 127 from the runtime) or did not answer: the device was not made with it.
		return &ObserveError{Reason: "the in-device tool is missing or did not answer"}
	}
	return nil
}

func (o *execObserver) Exec(ctx context.Context, s ExecSpec) (ExecResult, error) {
	var resp probe.ExecResponse
	req := probe.ExecRequest{Argv: s.Command, Script: s.Script, Interpreter: s.Interpreter, User: s.User, TimeoutMs: s.Limit.Milliseconds()}
	if err := o.ask(ctx, probe.CommandCheckExec, req, &resp); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Started: resp.Started, Exit: resp.Exit, Signal: resp.Signal, TimedOut: resp.TimedOut, Output: resp.Output, Truncated: resp.Truncated}, nil
}

func (o *execObserver) ReadFile(ctx context.Context, path string, wantContent bool) (FileFacts, error) {
	var resp probe.FileResponse
	if err := o.ask(ctx, probe.CommandCheckFile, probe.FileRequest{Path: path, WantContent: wantContent}, &resp); err != nil {
		return FileFacts{}, err
	}
	if resp.Error != "" {
		return FileFacts{}, &ObserveError{Reason: "the file could not be read in the device: " + resp.Error}
	}
	return FileFacts{Exists: resp.Exists, Regular: resp.Regular, Type: resp.Type, Mode: resp.Mode, UID: resp.UID, GID: resp.GID, User: resp.User, Group: resp.Group,
		Content: resp.Content, Truncated: resp.Truncated}, nil
}
