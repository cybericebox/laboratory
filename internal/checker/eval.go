package checker

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/cybericebox/laboratory/internal/labchecks"
)

// Outcome is the verdict of one run of one check.
type Outcome int

const (
	// Passed: every part held in this run.
	Passed Outcome = iota + 1
	// NotPassed: at least one part did not hold.
	NotPassed
	// Failed: the check could not be made (the device could not be reached, a command did not start or ran over its time limit,
	// a file was too big to decide). It is published as the state "error".
	Failed
)

func (o Outcome) String() string {
	switch o {
	case Passed:
		return "passed"
	case NotPassed:
		return "not_passed"
	case Failed:
		return "error"
	}
	return "unknown"
}

// Run is the result of one run: the verdict, a short detail for the author and the teacher, and an optional value.
//
// Detail names the part and the kind of expectation that did not hold ("part 2 (file): content_contains not found") and the
// facts observed that are no secret of the check (an exit code, a mode). It NEVER holds an expected value of the checks document
// and never the output of a command or the content of a file: the output may hold what the participant has to find on their
// own, and text of the participant's device must not travel into the status and the stream.
type Run struct {
	Outcome Outcome
	Detail  string
	Value   string
}

// FileFacts of a device are produced by a FileReader; ExecResult by an Executor. Both are small interfaces so that the evaluation
// is the same for a container (read from the node) and a micro-VM (asked of the in-device tool), and testable anywhere.

// ExecResult is the outcome of running a command in a device.
type ExecResult struct {
	Started   bool
	Exit      int
	Signal    string
	TimedOut  bool
	Output    []byte
	Truncated bool
}

var patterns sync.Map // string -> *regexp.Regexp

// compile caches the compiled patterns: a check runs again and again.
func compile(expr string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(expr); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	patterns.Store(expr, re)
	return re, nil
}

// partVerdict is the evaluation of one part: Passed, NotPassed or Failed with the reasons.
type partVerdict struct {
	outcome Outcome
	reasons []string
}

// EvalFile compares what a reader saw of a file with the expectations of the part. Every expectation must hold. A file that
// is too large to decide (the expectation was not found in the part that was read) is Failed, not NotPassed: the checker does not know.
func EvalFile(p *labchecks.FilePart, f FileFacts) (Outcome, []string) {
	v := evalFile(p, f)
	return v.outcome, v.reasons
}

func evalFile(p *labchecks.FilePart, f FileFacts) partVerdict {
	if p.Exists != nil && !*p.Exists {
		if f.Exists {
			return partVerdict{NotPassed, []string{"exists: the file is present"}}
		}
		return partVerdict{outcome: Passed}
	}
	if !f.Exists {
		return partVerdict{NotPassed, []string{"exists: the file is missing"}}
	}
	var fails []string
	undecided := false
	if p.ContentContains != nil || p.ContentMatches != nil {
		switch {
		case !f.Regular:
			fails = append(fails, "content: not a regular file")
		default:
			if p.ContentContains != nil && !bytes.Contains(f.Content, []byte(*p.ContentContains)) {
				fails = append(fails, "content_contains: not found")
				undecided = undecided || f.Truncated
			}
			if p.ContentMatches != nil {
				re, err := compile(*p.ContentMatches)
				switch {
				case err != nil:
					return partVerdict{Failed, []string{"content_matches: the pattern does not compile"}}
				case !re.Match(f.Content):
					fails = append(fails, "content_matches: no match")
					undecided = undecided || f.Truncated
				}
			}
		}
	}
	if p.Mode != nil && f.Mode != *p.Mode {
		fails = append(fails, fmt.Sprintf("mode: is %04o", f.Mode))
	}
	if p.Owner != nil && !ownerMatches(*p.Owner, f.UID, f.User, f.Unmapped) {
		fails = append(fails, "owner: differs"+unmappedNote(f.Unmapped))
	}
	if p.Group != nil && !ownerMatches(*p.Group, f.GID, f.Group, f.Unmapped) {
		fails = append(fails, "group: differs"+unmappedNote(f.Unmapped))
	}
	switch {
	case len(fails) == 0:
		return partVerdict{outcome: Passed}
	case undecided && onlyContentFails(fails):
		return partVerdict{Failed, []string{"content: the file is larger than the part that is read"}}
	}
	return partVerdict{NotPassed, fails}
}

func unmappedNote(unmapped bool) string {
	if unmapped {
		return " (owner unknown to the container)"
	}
	return ""
}

func onlyContentFails(fails []string) bool {
	for _, f := range fails {
		if !strings.HasPrefix(f, "content") {
			return false
		}
	}
	return true
}

// ownerMatches: a number is compared with the numeric id, anything else with the name in the device's passwd or group file.
func ownerMatches(want string, id uint32, name string, unmapped bool) bool {
	if unmapped {
		return false
	}
	if n, ok := atoiStrict(want); ok {
		return uint64(id) == n
	}
	return name != "" && name == want
}

func atoiStrict(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' || n > 1<<32 {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}

// EvalCustom judges the outcome of a command: exit code 0 passes, then the optional output expectations must hold. A command that did not
// start or ran over its time limit is Failed. A command killed by a signal did not pass.
func EvalCustom(p *labchecks.CustomPart, r ExecResult) (Outcome, []string) {
	v := evalCustom(p, r)
	return v.outcome, v.reasons
}

func evalCustom(p *labchecks.CustomPart, r ExecResult) partVerdict {
	switch {
	case !r.Started:
		return partVerdict{Failed, []string{"the command did not start"}}
	case r.TimedOut:
		return partVerdict{Failed, []string{"the command ran over its time limit"}}
	case r.Signal != "":
		return partVerdict{NotPassed, []string{"the command was killed by " + r.Signal}}
	case r.Exit != 0:
		return partVerdict{NotPassed, []string{fmt.Sprintf("exit code %d", r.Exit)}}
	}
	var fails []string
	if p.OutputContains != nil && !bytes.Contains(r.Output, []byte(*p.OutputContains)) {
		fails = append(fails, "output_contains: not found")
	}
	if p.OutputMatches != nil {
		re, err := compile(*p.OutputMatches)
		switch {
		case err != nil:
			return partVerdict{Failed, []string{"output_matches: the pattern does not compile"}}
		case !re.Match(r.Output):
			fails = append(fails, "output_matches: no match")
		}
	}
	switch {
	case len(fails) == 0:
		return partVerdict{outcome: Passed}
	case r.Truncated:
		return partVerdict{Failed, []string{"output: longer than the part that is read"}}
	}
	return partVerdict{NotPassed, fails}
}

// combine joins the verdicts of the parts of one check: all parts must hold in the same run. A part that did not hold decides the
// check (a conjunction with a false part is false whatever the others say); otherwise a part that could not be made makes the
// whole run Failed; otherwise it passed.
func combine(verdicts []partVerdict, kinds []string) Run {
	var notPassed, failed []string
	for i, v := range verdicts {
		for _, r := range v.reasons {
			line := fmt.Sprintf("part %d (%s): %s", i+1, kinds[i], r)
			if v.outcome == Failed {
				failed = append(failed, line)
			} else {
				notPassed = append(notPassed, line)
			}
		}
	}
	switch {
	case len(notPassed) > 0:
		return Run{Outcome: NotPassed, Detail: labchecks.TruncateDetail(strings.Join(notPassed, "; "))}
	case len(failed) > 0:
		return Run{Outcome: Failed, Detail: labchecks.TruncateDetail(strings.Join(failed, "; "))}
	}
	return Run{Outcome: Passed}
}
