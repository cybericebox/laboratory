package laboratory

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/egressproxy"
)

func writeDay(t *testing.T, dir, day string, rows ...EgressLogRow) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "egress-"+day+".jsonl"), b.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}

// The days that are over are gzipped, the open day stays plain, the days past the retention go; the search reads both kinds.
func TestEgressLogDaysAreCompressedPrunedAndSearchable(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	at := func(day string, h int) time.Time {
		d, _ := time.Parse(egressLogDay, day)
		return d.Add(time.Duration(h) * time.Hour)
	}
	writeDay(t, dir, "2026-09-01", EgressLogRow{Minute: at("2026-09-01", 1), Namespace: "lg-a", Lab: "l1", Host: "old.example", Conns: 1})
	writeDay(t, dir, "2026-11-01", EgressLogRow{Minute: at("2026-11-01", 3), Namespace: "lg-a", Lab: "l1", Host: "pypi.org", Conns: 5, Up: 1, Down: 2},
		EgressLogRow{Minute: at("2026-11-01", 4), Namespace: "lg-b", Lab: "l2", Host: "files.pythonhosted.org", Conns: 2})
	writeDay(t, dir, "2026-11-03", EgressLogRow{Minute: at("2026-11-03", 9), Namespace: "lg-a", Lab: "l1", Host: "pypi.org", Conns: 1})
	m := &EgressMonitor{LogDir: dir, Now: func() time.Time { return now }}
	if err := m.pruneLog(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"egress-2026-09-01.jsonl": false, "egress-2026-09-01.jsonl.gz": false, // past the retention
		"egress-2026-11-01.jsonl": false, "egress-2026-11-01.jsonl.gz": true, // over: compressed, the plain file removed
		"egress-2026-11-03.jsonl": true, "egress-2026-11-03.jsonl.gz": false, // today: plain
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s: present=%v, want %v", name, err == nil, want)
		}
	}
	search := func(q EgressLogQuery) string {
		var b bytes.Buffer
		if _, err := SearchEgressLog(dir, q, &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if out := search(EgressLogQuery{Host: "pypi.org"}); strings.Count(out, "\n") != 2 {
		t.Errorf("pypi.org is in a compressed and in an open day: %q", out)
	}
	if out := search(EgressLogQuery{Host: "*.pythonhosted.org", Lab: "l2"}); strings.Count(out, "\n") != 1 || !strings.Contains(out, "files.pythonhosted.org") {
		t.Errorf("suffix and lab: %q", out)
	}
	if out := search(EgressLogQuery{Namespace: "lg-b", Host: "pypi.org"}); out != "" {
		t.Errorf("a group without that host: %q", out)
	}
	if out := search(EgressLogQuery{From: at("2026-11-03", 0), To: at("2026-11-04", 0)}); strings.Count(out, "\n") != 1 {
		t.Errorf("a time range reads only its days: %q", out)
	}
	if out := search(EgressLogQuery{Limit: 1}); strings.Count(out, "\n") != 1 {
		t.Errorf("the limit: %q", out)
	}
}

// After a restart the operator goes on where the log ends: the minutes the gateways kept meanwhile are pulled once, none twice.
func TestEgressMonitorTakesUpWhereTheLogEnds(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	dir := t.TempDir()
	f.m.LogDir = dir
	base := time.Date(2026, 11, 1, 11, 30, 0, 0, time.UTC)
	writeDay(t, dir, "2026-11-01", EgressLogRow{Minute: base, Namespace: "lg-a", Lab: "l1", Host: "pypi.org", Conns: 3})
	*f.minutes = []egressproxy.Minute{
		{Start: base, Rows: []egressproxy.Row{{Lab: "l1", Host: "pypi.org", Conns: 3}}},                       // already in the log
		{Start: base.Add(time.Minute), Rows: []egressproxy.Row{{Lab: "l1", Host: "pypi.org", Conns: 4}}},      // missed while the operator was down
		{Start: base.Add(20 * time.Minute), Rows: []egressproxy.Row{{Lab: "l1", Host: "pypi.org", Conns: 5}}}, // the later part of the gap
	}
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	n, err := SearchEgressLog(dir, EgressLogQuery{Host: "pypi.org"}, &b)
	if err != nil || n != 3 {
		t.Fatalf("the log holds the old minute once and the two missed ones: %d rows %v\n%s", n, err, b.String())
	}
}
