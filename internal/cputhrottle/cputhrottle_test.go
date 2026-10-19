package cputhrottle

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	text := "usage_usec 100\nuser_usec 60\nsystem_usec 40\nnr_periods 200\nnr_throttled 150\nthrottled_usec 9000\nnr_bursts 0\n"
	got, err := ParseStat(bufio.NewScanner(strings.NewReader(text)))
	if err != nil || got != (Stat{Periods: 200, Throttled: 150}) {
		t.Fatalf("%+v %v", got, err)
	}
	// A cgroup with no CPU limit has no counters: that is an error, not a zero ratio.
	if _, err := ParseStat(bufio.NewScanner(strings.NewReader("usage_usec 1\n"))); err == nil {
		t.Fatal("no counters accepted")
	}
	if _, err := ParseStat(bufio.NewScanner(strings.NewReader("nr_periods x\nnr_throttled 1\n"))); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestReadStatFromCgroupDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("nr_periods 40\nnr_throttled 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadStat(dir); err != nil || got.Throttled != 30 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ReadStat(t.TempDir()); err == nil {
		t.Fatal("a missing file is an error")
	}
}

func TestWindowPercentOverTheLastFiveMinutes(t *testing.T) {
	var w Window
	t0 := time.Unix(1000, 0)
	if _, _, ok := w.Percent(); ok {
		t.Fatal("empty window has a ratio")
	}
	w.Add(t0, Stat{Periods: 1000, Throttled: 0})
	w.Add(t0.Add(30*time.Second), Stat{Periods: 1010, Throttled: 9})
	if _, _, ok := w.Percent(); ok {
		t.Fatal("ten periods are too few to say")
	}
	w.Add(t0.Add(60*time.Second), Stat{Periods: 1100, Throttled: 70})
	if p, covered, ok := w.Percent(); !ok || p != 70 || covered != time.Minute {
		t.Fatalf("70 of 100 periods: %d%% over %v (ok %v)", p, covered, ok)
	}

	// Sampled every 30 s while the container stops being throttled: ten minutes later only the last five count.
	periods, throttled := uint64(1100), uint64(70)
	for at := 90 * time.Second; at <= 11*time.Minute; at += 30 * time.Second {
		periods += 50
		w.Add(t0.Add(at), Stat{Periods: periods, Throttled: throttled})
	}
	p, covered, ok := w.Percent()
	if !ok || p != 0 || covered > WindowSpan+30*time.Second || covered < WindowSpan-time.Minute {
		t.Fatalf("after the window moved: %d%% over %v (ok %v)", p, covered, ok)
	}
}

func TestWindowRoundsToTheStepAndCapsAtHundred(t *testing.T) {
	var w Window
	t0 := time.Unix(0, 0)
	w.Add(t0, Stat{})
	w.Add(t0.Add(time.Minute), Stat{Periods: 100, Throttled: 54})
	if p, _, _ := w.Percent(); p != 50 {
		t.Fatalf("54%% reported as %d", p)
	}
	w.Add(t0.Add(2*time.Minute), Stat{Periods: 200, Throttled: 200})
	if p, _, _ := w.Percent(); p != 100 {
		t.Fatalf("full throttle reported as %d", p)
	}
}

func TestWindowStartsOverWhenTheCountersGoBack(t *testing.T) {
	var w Window
	t0 := time.Unix(0, 0)
	w.Add(t0, Stat{Periods: 5000, Throttled: 4000})
	w.Add(t0.Add(time.Minute), Stat{Periods: 5100, Throttled: 4100})
	// the container restarted: new cgroup, counters from zero
	w.Add(t0.Add(2*time.Minute), Stat{Periods: 10, Throttled: 0})
	if _, _, ok := w.Percent(); ok {
		t.Fatal("history of the old container leaked into the new one")
	}
	w.Add(t0.Add(3*time.Minute), Stat{Periods: 110, Throttled: 0})
	if p, _, ok := w.Percent(); !ok || p != 0 {
		t.Fatalf("%d %v", p, ok)
	}
}
