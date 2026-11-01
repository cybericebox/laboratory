package manager

import (
	"testing"
	"time"
)

func TestParseLogTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"": {}, "2026-11-01": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "2026-11-01T10:30:00Z": time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC),
	} {
		if got, err := parseLogTime(in); err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := parseLogTime("yesterday"); err == nil {
		t.Error("a bad time must be refused")
	}
}

func TestRunEgressLogNeedsADirectory(t *testing.T) {
	t.Setenv("EGRESS_LOG_DIR", "")
	if code := RunEgressLog(nil); code != 2 {
		t.Errorf("without a directory the command refuses: %d", code)
	}
	if code := RunEgressLog([]string{"--dir", t.TempDir(), "--host", "pypi.org"}); code != 0 {
		t.Errorf("an empty log is an empty answer: %d", code)
	}
}
