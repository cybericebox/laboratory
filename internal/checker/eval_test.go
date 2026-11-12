package checker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
)

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }
func mp(m uint32) *uint32 { return &m }
func present(content string) FileFacts {
	return FileFacts{Exists: true, Regular: true, Mode: 0o640, UID: 0, GID: 33, User: "root", Group: "www-data", Content: []byte(content)}
}

func TestEvalFile(t *testing.T) {
	long := present("abc")
	long.Truncated = true
	notRegular := FileFacts{Exists: true, Mode: 0o755, User: "root", Group: "root"}
	unmapped := present("x")
	unmapped.Unmapped = true
	for _, c := range []struct {
		name string
		part labchecks.FilePart
		f    FileFacts
		want Outcome
		hint string
	}{
		{"exists and there", labchecks.FilePart{Exists: bp(true)}, present(""), Passed, ""},
		{"exists but missing", labchecks.FilePart{Exists: bp(true)}, FileFacts{}, NotPassed, "missing"},
		{"absent and absent", labchecks.FilePart{Exists: bp(false)}, FileFacts{}, Passed, ""},
		{"absent but there", labchecks.FilePart{Exists: bp(false)}, present(""), NotPassed, "present"},
		{"any expectation needs the file", labchecks.FilePart{Mode: mp(0o600)}, FileFacts{}, NotPassed, "missing"},
		{"contains yes", labchecks.FilePart{ContentContains: sp("PermitRootLogin no")}, present("a\nPermitRootLogin no\nb"), Passed, ""},
		{"contains no", labchecks.FilePart{ContentContains: sp("PermitRootLogin no")}, present("PermitRootLogin yes"), NotPassed, "content_contains"},
		{"matches yes", labchecks.FilePart{ContentMatches: sp(`(?m)^PermitRootLogin\s+no$`)}, present("x\nPermitRootLogin   no\n"), Passed, ""},
		{"matches no", labchecks.FilePart{ContentMatches: sp(`^PermitRootLogin\s+no$`)}, present("PermitRootLogin yes"), NotPassed, "content_matches"},
		{"content of a directory", labchecks.FilePart{ContentContains: sp("x")}, notRegular, NotPassed, "regular"},
		{"mode yes", labchecks.FilePart{Mode: mp(0o640)}, present(""), Passed, ""},
		{"mode no", labchecks.FilePart{Mode: mp(0o600)}, present(""), NotPassed, "mode"},
		{"owner by name", labchecks.FilePart{Owner: sp("root")}, present(""), Passed, ""},
		{"owner by number", labchecks.FilePart{Owner: sp("0")}, present(""), Passed, ""},
		{"owner other", labchecks.FilePart{Owner: sp("web")}, present(""), NotPassed, "owner"},
		{"owner name unknown to the device", labchecks.FilePart{Owner: sp("web")}, FileFacts{Exists: true, Regular: true, UID: 1000}, NotPassed, "owner"},
		{"group by name", labchecks.FilePart{Group: sp("www-data")}, present(""), Passed, ""},
		{"group by number", labchecks.FilePart{Group: sp("33")}, present(""), Passed, ""},
		{"owner unmapped", labchecks.FilePart{Owner: sp("0")}, unmapped, NotPassed, "unknown to the container"},
		{"all must hold", labchecks.FilePart{Exists: bp(true), ContentContains: sp("a"), Mode: mp(0o600)}, present("a"), NotPassed, "mode"},
		// A file over the size cap decides nothing about its content, found or not: an anchor in a pattern would fit the cut.
		{"truncated and found", labchecks.FilePart{ContentContains: sp("abc")}, long, Failed, "larger"},
		{"truncated and not found", labchecks.FilePart{ContentContains: sp("zzz")}, long, Failed, "larger"},
		{"truncated, matches", labchecks.FilePart{ContentMatches: sp("abc$")}, long, Failed, "larger"},
		{"truncated, checksum", labchecks.FilePart{Checksum: sp(strings.Repeat("0", 64))}, long, Failed, "larger"},
		{"truncated, mode wrong is decided", labchecks.FilePart{ContentContains: sp("zzz"), Mode: mp(0o600)}, long, NotPassed, "mode"},
		{"truncated, mode right, content undecided", labchecks.FilePart{ContentContains: sp("abc"), Mode: mp(0o640)}, long, Failed, "larger"},
		{"truncated, no content rule at all", labchecks.FilePart{Mode: mp(0o640)}, long, Passed, ""},
		{"exists:false on a present truncated file", labchecks.FilePart{Exists: bp(false)}, long, NotPassed, "present"},
		{"bad pattern", labchecks.FilePart{ContentMatches: sp("(")}, present("x"), Failed, "compile"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, reasons := EvalFile(&c.part, c.f)
			if got != c.want || (c.hint != "" && !strings.Contains(strings.Join(reasons, ";"), c.hint)) {
				t.Fatalf("got %v %v, want %v containing %q", got, reasons, c.want, c.hint)
			}
		})
	}
}

func TestEvalCustom(t *testing.T) {
	ok := ExecResult{Started: true, Output: []byte("service active\n")}
	for _, c := range []struct {
		name string
		part labchecks.CustomPart
		r    ExecResult
		want Outcome
		hint string
	}{
		{"exit 0", labchecks.CustomPart{}, ok, Passed, ""},
		{"exit 1", labchecks.CustomPart{}, ExecResult{Started: true, Exit: 1}, NotPassed, "exit code 1"},
		{"did not start", labchecks.CustomPart{}, ExecResult{}, Failed, "did not start"},
		{"timed out", labchecks.CustomPart{}, ExecResult{Started: true, TimedOut: true, Exit: -1}, Failed, "time limit"},
		{"killed", labchecks.CustomPart{}, ExecResult{Started: true, Exit: -1, Signal: "segmentation fault"}, NotPassed, "killed"},
		{"output contains yes", labchecks.CustomPart{OutputContains: sp("active")}, ok, Passed, ""},
		{"output contains no", labchecks.CustomPart{OutputContains: sp("inactive")}, ok, NotPassed, "output_contains"},
		{"output matches yes", labchecks.CustomPart{OutputMatches: sp(`^service\s+active$`)}, ExecResult{Started: true, Output: []byte("service active")}, Passed, ""},
		{"output matches no", labchecks.CustomPart{OutputMatches: sp(`^inactive`)}, ok, NotPassed, "output_matches"},
		{"exit code decides before output", labchecks.CustomPart{OutputContains: sp("active")}, ExecResult{Started: true, Exit: 3, Output: []byte("active")}, NotPassed, "exit code 3"},
		{"truncated output, contains not found", labchecks.CustomPart{OutputContains: sp("zzz")}, ExecResult{Started: true, Output: []byte("a"), Truncated: true}, Failed, "longer"},
		{"truncated output, contains found", labchecks.CustomPart{OutputContains: sp("a")}, ExecResult{Started: true, Output: []byte("a"), Truncated: true}, Failed, "longer"},
		{"truncated output, matches", labchecks.CustomPart{OutputMatches: sp("a$")}, ExecResult{Started: true, Output: []byte("a"), Truncated: true}, Failed, "longer"},
		{"truncated output, no output rule, the exit code decides", labchecks.CustomPart{}, ExecResult{Started: true, Output: []byte("a"), Truncated: true}, Passed, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, reasons := EvalCustom(&c.part, c.r)
			if got != c.want || (c.hint != "" && !strings.Contains(strings.Join(reasons, ";"), c.hint)) {
				t.Fatalf("got %v %v, want %v containing %q", got, reasons, c.want, c.hint)
			}
		})
	}
}

// fakeObserver answers from tables.
type fakeObserver struct {
	mu    sync.Mutex
	files map[string]FileFacts
	exec  func(ExecSpec) (ExecResult, error)
	calls []string
}

func (f *fakeObserver) set(path string, facts FileFacts) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = facts
}

func (f *fakeObserver) ReadFile(_ context.Context, p string, _ bool) (FileFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "file "+p)
	facts, ok := f.files[p]
	if !ok {
		return FileFacts{}, nil
	}
	return facts, nil
}

func (f *fakeObserver) Exec(_ context.Context, s ExecSpec) (ExecResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "exec")
	f.mu.Unlock()
	if f.exec == nil {
		return ExecResult{Started: true}, nil
	}
	return f.exec(s)
}

func check(parts ...labchecks.Part) *labchecks.Check {
	return &labchecks.Check{ID: "c", Device: "d", Mode: "continuous", TimeLimitSeconds: 5, Parts: parts}
}

func TestRunCheckAllPartsMustHoldInOneRun(t *testing.T) {
	obs := &fakeObserver{files: map[string]FileFacts{"/etc/a": present("on"), "/etc/b": present("off")}}
	file := func(p string, contains string) labchecks.Part {
		return labchecks.Part{File: &labchecks.FilePart{Path: p, ContentContains: sp(contains)}}
	}
	if r := RunCheck(context.Background(), check(file("/etc/a", "on"), file("/etc/b", "off"), labchecks.Part{Custom: &labchecks.CustomPart{Command: []string{"true"}}}), obs); r.Outcome != Passed || r.Detail != "" {
		t.Fatalf("all hold: %+v", r)
	}
	r := RunCheck(context.Background(), check(file("/etc/a", "on"), file("/etc/b", "on")), obs)
	if r.Outcome != NotPassed || r.Detail != "part 2 (file): content_contains: not found" {
		t.Fatalf("second part fails: %+v", r)
	}
}

func TestRunCheckNotPassedDecidesOverFailed(t *testing.T) {
	obs := &fakeObserver{files: map[string]FileFacts{}, exec: func(ExecSpec) (ExecResult, error) { return ExecResult{}, nil }}
	r := RunCheck(context.Background(), check(
		labchecks.Part{Custom: &labchecks.CustomPart{Command: []string{"x"}}},
		labchecks.Part{File: &labchecks.FilePart{Path: "/missing", Exists: bp(true)}}), obs)
	if r.Outcome != NotPassed {
		t.Fatalf("a part that is false decides: %+v", r)
	}
	r = RunCheck(context.Background(), check(labchecks.Part{Custom: &labchecks.CustomPart{Command: []string{"x"}}}), obs)
	if r.Outcome != Failed {
		t.Fatalf("not started: %+v", r)
	}
}

// The detail never carries an expected value of the checks document, nor the output of a command or the content of a file.
func TestDetailHoldsNoExpectedValueNorOutput(t *testing.T) {
	secret := "ICE{expected-flag-value}"
	obs := &fakeObserver{
		files: map[string]FileFacts{"/f": present("the content holds " + secret + " too")},
		exec: func(ExecSpec) (ExecResult, error) {
			return ExecResult{Started: true, Exit: 2, Output: []byte("output with " + secret)}, nil
		},
	}
	r := RunCheck(context.Background(), check(
		labchecks.Part{File: &labchecks.FilePart{Path: "/f", ContentContains: sp("EXPECTED-CONTENT-" + secret), ContentMatches: sp("EXPECTED-RE"), Owner: sp("EXPECTED-OWNER"), Mode: mp(0o123)}},
		labchecks.Part{Custom: &labchecks.CustomPart{Command: []string{"EXPECTED-CMD"}, OutputContains: sp("EXPECTED-OUT")}}), obs)
	if r.Outcome != NotPassed {
		t.Fatalf("got %+v", r)
	}
	for _, bad := range []string{"EXPECTED", secret, "0123", "123"} {
		if strings.Contains(r.Detail, bad) {
			t.Errorf("the detail %q holds %q", r.Detail, bad)
		}
	}
}

func TestObserverErrorsBecomeSafeReasons(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrPseudoPath, "pseudo file system"},
		{ErrTooManyLinks, "too many symbolic links"},
		{&ObserveError{Reason: "the container is not running", Err: errors.New("rpc: /run/containerd/x.sock secret path")}, "the container is not running"},
		{errors.New("open /run/containerd/io.containerd/abc: permission denied"), "could not be observed"},
	} {
		obs := &fakeErr{c.err}
		r := RunCheck(context.Background(), check(labchecks.Part{File: &labchecks.FilePart{Path: "/f", Exists: bp(true)}}), obs)
		if r.Outcome != Failed || !strings.Contains(r.Detail, c.want) || strings.Contains(r.Detail, "/run/") {
			t.Errorf("%v: %+v", c.err, r)
		}
	}
}

type fakeErr struct{ err error }

func (f *fakeErr) ReadFile(context.Context, string, bool) (FileFacts, error) {
	return FileFacts{}, f.err
}
func (f *fakeErr) Exec(context.Context, ExecSpec) (ExecResult, error) { return ExecResult{}, f.err }

func TestRunCheckTimeLimitCoversTheWholeRun(t *testing.T) {
	var limits []time.Duration
	obs := &fakeObserver{exec: func(s ExecSpec) (ExecResult, error) {
		limits = append(limits, s.Limit)
		return ExecResult{Started: true}, nil
	}}
	c := check(labchecks.Part{Custom: &labchecks.CustomPart{Command: []string{"a"}}})
	c.TimeLimitSeconds = 30
	RunCheck(context.Background(), c, obs)
	if len(limits) != 1 || limits[0] > 30*time.Second || limits[0] < 29*time.Second {
		t.Fatalf("the command gets what is left of the 30 s: %v", limits)
	}
}

func TestRunCheckDeadlineIsAFailure(t *testing.T) {
	obs := &blockingObserver{}
	c := check(labchecks.Part{File: &labchecks.FilePart{Path: "/f", Exists: bp(true)}})
	c.TimeLimitSeconds = 1
	start := time.Now()
	r := RunCheck(context.Background(), c, obs)
	if r.Outcome != Failed || !strings.Contains(r.Detail, "time limit") || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

type blockingObserver struct{}

func (blockingObserver) ReadFile(ctx context.Context, _ string, _ bool) (FileFacts, error) {
	<-ctx.Done()
	return FileFacts{}, ctx.Err()
}
func (blockingObserver) Exec(ctx context.Context, _ ExecSpec) (ExecResult, error) {
	<-ctx.Done()
	return ExecResult{}, ctx.Err()
}
