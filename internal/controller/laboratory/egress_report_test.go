package laboratory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/egressproxy"
)

func rowMinute(start time.Time, host string, conns int64) egressproxy.Minute {
	return egressproxy.Minute{Start: start, Rows: []egressproxy.Row{{Lab: "l1", Host: host, Conns: conns}}}
}

func TestEgressReportIsAcknowledgedThroughTheNewestLoggedMinute(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	f.m.LogDir = t.TempDir()
	base := time.Date(2026, 11, 1, 11, 50, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", rowMinute(base, "a.test", 1), rowMinute(base.Add(time.Minute), "b.test", 1), rowMinute(base.Add(2*time.Minute), "c.test", 1))
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.report(t, "lg-a")
	if r.Spec.AckedThrough == nil || !r.Spec.AckedThrough.Time.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("ackedThrough = %v, want %v", r.Spec.AckedThrough, base.Add(2*time.Minute))
	}
	// a second pull with nothing new neither logs again nor moves the acknowledgement
	rv := r.ResourceVersion
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.report(t, "lg-a"); got.ResourceVersion != rv {
		t.Errorf("an acknowledgement that has not moved must not be written again (resourceVersion %s -> %s)", rv, got.ResourceVersion)
	}
	b, _ := os.ReadFile(filepath.Join(f.m.LogDir, "egress-2026-11-01.jsonl"))
	if strings.Count(string(b), "\n") != 3 {
		t.Fatalf("each minute is logged once: %q", b)
	}
	// new minutes move it on, once per pull
	f.setMinutes(t, "lg-a", "gateway", rowMinute(base.Add(2*time.Minute), "c.test", 1), rowMinute(base.Add(3*time.Minute), "d.test", 1), rowMinute(base.Add(4*time.Minute), "e.test", 1))
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.report(t, "lg-a"); !r.Spec.AckedThrough.Time.Equal(base.Add(4 * time.Minute)) {
		t.Fatalf("ackedThrough = %v", r.Spec.AckedThrough)
	}
}

// The log is written before anything is acknowledged: when it cannot be written the gateway keeps the minutes, and the next pull reads them.
func TestEgressReportIsNotAcknowledgedWhenTheLogCannotBeWritten(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.m.LogDir = filepath.Join(blocker, "log") // a directory below a regular file cannot be made
	base := time.Date(2026, 11, 1, 11, 50, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", rowMinute(base, "a.test", 1))
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.report(t, "lg-a"); r.Spec.AckedThrough != nil {
		t.Fatalf("nothing is acknowledged that is not in the log: %v", r.Spec.AckedThrough)
	}
	// the disk comes back: the same minute is read again and logged, then acknowledged
	f.m.LogDir = t.TempDir()
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f.m.LogDir, "egress-2026-11-01.jsonl"))
	if !strings.Contains(string(b), "a.test") {
		t.Fatalf("the minute is lost: %q", b)
	}
	if r := f.report(t, "lg-a"); r.Spec.AckedThrough == nil || !r.Spec.AckedThrough.Time.Equal(base) {
		t.Fatalf("ackedThrough = %v", r.Spec.AckedThrough)
	}
}

// An operator that restarts goes on after what is acknowledged (and after what its log holds): a minute is never logged twice, and a
// minute the gateway kept while the operator was away is logged.
func TestEgressMonitorRestartCatchesUpFromTheAcknowledgement(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	base := time.Date(2026, 11, 1, 11, 50, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", rowMinute(base, "old.test", 1), rowMinute(base.Add(time.Minute), "kept.test", 1), rowMinute(base.Add(2*time.Minute), "kept2.test", 1))
	r := f.report(t, "lg-a")
	at := metav1.NewTime(base)
	r.Spec.AckedThrough = &at // a previous operator logged the first minute and acknowledged it, then died before the gateway trimmed it
	if err := f.c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f.m.LogDir = dir // a new operator with an empty directory: only the acknowledgement tells it where to start
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "egress-2026-11-01.jsonl"))
	if strings.Contains(string(b), "old.test") || !strings.Contains(string(b), "kept.test") || !strings.Contains(string(b), "kept2.test") {
		t.Fatalf("the acknowledged minute is skipped, the two kept ones are logged: %q", b)
	}
	if r := f.report(t, "lg-a"); !r.Spec.AckedThrough.Time.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("ackedThrough = %v", r.Spec.AckedThrough)
	}
}

// The acknowledgement only moves forward: a report that carries an older minute than the acknowledgement does not pull it back.
func TestEgressAcknowledgementNeverMovesBack(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	base := time.Date(2026, 11, 1, 11, 50, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", rowMinute(base, "a.test", 1))
	r := f.report(t, "lg-a")
	later := metav1.NewTime(base.Add(10 * time.Minute))
	r.Spec.AckedThrough = &later
	if err := f.c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.report(t, "lg-a"); !r.Spec.AckedThrough.Time.Equal(base.Add(10 * time.Minute)) {
		t.Fatalf("ackedThrough moved back to %v", r.Spec.AckedThrough)
	}
}

// What the report holds is bounded again by the monitor: the limits of accept() apply to a report as they did to a page.
func TestEgressMonitorAppliesTheTrustLimitsToAReport(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	f.m.LogDir = t.TempDir()
	now := f.m.now()
	f.setMinutes(t, "lg-a", "gateway",
		rowMinute(now.Add(-3*time.Hour).Truncate(time.Minute), "ancient.test", 1),
		rowMinute(now.Add(time.Hour).Truncate(time.Minute), "future.test", 1),
		rowMinute(now.Add(-2*time.Minute).Truncate(time.Minute), "fine.test", 1),
	)
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f.m.LogDir, "egress-2026-11-01.jsonl"))
	if !strings.Contains(string(b), "fine.test") || strings.Contains(string(b), "future.test") || strings.Contains(string(b), "ancient.test") {
		t.Fatalf("log = %q", b)
	}
	if r := f.report(t, "lg-a"); r.Spec.AckedThrough == nil || !r.Spec.AckedThrough.Time.Equal(now.Add(-2*time.Minute).Truncate(time.Minute)) {
		t.Fatalf("only a minute that was logged is acknowledged: %v", r.Spec.AckedThrough)
	}
}

// A group whose report is gone is forgotten (no cursor kept for it), and a group with no report yet does no harm.
func TestEgressMonitorWithNoReportDoesNothing(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.setMinutes(t, "lg-a", "gateway", rowMinute(time.Date(2026, 11, 1, 11, 58, 0, 0, time.UTC), "x.test", 1))
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r laboratoryv1alpha1.LabEgressReport
	r.Name, r.Namespace = "gateway", "lg-a"
	if err := f.c.Delete(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.m.last["lg-a"]; ok {
		t.Error("the cursor of a group without a report must be forgotten")
	}
}
