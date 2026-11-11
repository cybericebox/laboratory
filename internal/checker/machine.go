package checker

import (
	"time"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
)

// ErrorsBeforePublish is how many scheduled runs in a row must fail (state error) before "error" replaces a result that is already
// published as passed or not_passed. A manual run publishes an error at once.
//
// What is written on error, precisely:
//   - no result published yet for the check: "error" with its detail, at once;
//   - a passed / not_passed result is published: nothing is written for the first ErrorsBeforePublish-1 failed scheduled runs, so a
//     hiccup of the device (a restart, a slow exec) never touches what the participant has earned;
//   - after that, or at once for a manual run: state error with the detail, changedMs = now, firstPassedMs KEPT (it is never
//     cleared), value cleared. The consumer keeps the state of the previous change (contract section 3);
//   - the next run that gives passed or not_passed replaces the error. A once check that has passed never runs again, so an error
//     can never replace its pass.
const ErrorsBeforePublish = 3

// Published is one entry of LabStatus.checks.
type Published struct {
	State         laboratoryv1alpha1.CheckState
	FirstPassedMs int64
	ChangedMs     int64
	Value         string
	Detail        string
}

// PublishedFrom reads a result from the status of a lab.
func PublishedFrom(r laboratoryv1alpha1.CheckResult) Published {
	return Published{State: r.State, FirstPassedMs: r.FirstPassedMs, ChangedMs: r.ChangedMs, Value: r.Value, Detail: r.Detail}
}

// Result turns a published entry into the status entry of the check.
func (p Published) Result(id string) laboratoryv1alpha1.CheckResult {
	return laboratoryv1alpha1.CheckResult{ID: id, State: p.State, FirstPassedMs: p.FirstPassedMs, ChangedMs: p.ChangedMs, Value: p.Value, Detail: p.Detail}
}

func (p Published) definite() bool {
	return p.State == laboratoryv1alpha1.CheckPassed || p.State == laboratoryv1alpha1.CheckNotPassed
}

// machine is the state of one check: what is published and how many failed runs came in a row.
type machine struct {
	mode string
	pub  Published
	errs int
}

// finished reports a once check that has passed: it is not run again.
func (m *machine) finished() bool {
	return m.mode == string(laboratoryv1alpha1.CheckOnce) && m.pub.FirstPassedMs > 0
}

// reset forgets the failures in a row (the device was not ready in between).
func (m *machine) reset() { m.errs = 0 }

// apply folds a run into the state and says whether the published entry changed (and so has to be written). Only a change of the
// state, the value or the detail counts; a run that gives the same result changes nothing.
func (m *machine) apply(now time.Time, r Run, manual bool) bool {
	ms := now.UnixMilli()
	next := m.pub
	switch r.Outcome {
	case Passed, NotPassed:
		m.errs = 0
		next.State = laboratoryv1alpha1.CheckNotPassed
		if r.Outcome == Passed {
			next.State = laboratoryv1alpha1.CheckPassed
			if next.FirstPassedMs == 0 {
				next.FirstPassedMs = ms
			}
		}
		next.Value, next.Detail = labchecks.TruncateValue(r.Value), labchecks.TruncateDetail(r.Detail)
	case Failed:
		m.errs++
		if m.pub.definite() && !manual && m.errs < ErrorsBeforePublish {
			return false
		}
		next.State = laboratoryv1alpha1.CheckError
		next.Value, next.Detail = "", labchecks.TruncateDetail(r.Detail)
	default:
		return false
	}
	if next.State != m.pub.State {
		next.ChangedMs = ms
	}
	if next == m.pub {
		return false
	}
	m.pub = next
	return true
}
