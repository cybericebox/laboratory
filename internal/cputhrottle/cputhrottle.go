// Package cputhrottle measures how much the CPU limit of a container holds it back: the share of the CFS periods in which the
// container was throttled, over a sliding window of a few minutes, from the cpu.stat of its cgroup (v2).
package cputhrottle

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// WindowSpan is how far back the ratio looks.
	WindowSpan = 5 * time.Minute
	// MinPeriods is the fewest periods in a window a ratio is reported for: the counters only run while the container is
	// busy, and a few periods say nothing.
	MinPeriods = 30
	// Step is the granularity of the reported percent: a smaller change is not news.
	Step = 10
	// AlertPercent is the share above which a container counts as starved by its limit.
	AlertPercent = 50
)

// Stat is the throttling counters of a cgroup: periods elapsed with a quota in force, and how many of them throttled.
type Stat struct{ Periods, Throttled uint64 }

// ReadStat reads nr_periods and nr_throttled from the cpu.stat of the cgroup directory (cgroup v2).
func ReadStat(dir string) (Stat, error) {
	f, err := os.Open(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return Stat{}, err
	}
	defer func() { _ = f.Close() }()
	return ParseStat(bufio.NewScanner(f))
}

// ParseStat reads the counters from the lines of a cpu.stat. A cgroup without a CPU limit has no such counters.
func ParseStat(sc *bufio.Scanner) (Stat, error) {
	var s Stat
	seen := 0
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		var dst *uint64
		switch key {
		case "nr_periods":
			dst = &s.Periods
		case "nr_throttled":
			dst = &s.Throttled
		default:
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return Stat{}, fmt.Errorf("cpu.stat %s: %w", key, err)
		}
		*dst = n
		seen++
	}
	if err := sc.Err(); err != nil {
		return Stat{}, err
	}
	if seen < 2 {
		return Stat{}, fmt.Errorf("cpu.stat has no throttling counters")
	}
	return s, nil
}

type sample struct {
	at time.Time
	Stat
}

// Window keeps the samples of one container for WindowSpan.
type Window struct{ samples []sample }

// Add records the counters read at the given time. Counters that went back mean a new container: the history is dropped.
func (w *Window) Add(at time.Time, s Stat) {
	if n := len(w.samples); n > 0 && (s.Periods < w.samples[n-1].Periods || s.Throttled < w.samples[n-1].Throttled) {
		w.samples = nil
	}
	w.samples = append(w.samples, sample{at, s})
	// Keep one sample older than the span: it is the start of the window.
	for len(w.samples) > 2 && at.Sub(w.samples[1].at) >= WindowSpan {
		w.samples = w.samples[1:]
	}
}

// Percent is the share of throttled periods (0..100, rounded to Step) over the window and the time the window covers. ok is false
// while there are fewer than MinPeriods periods in it.
func (w *Window) Percent() (percent int32, covered time.Duration, ok bool) {
	if len(w.samples) < 2 {
		return 0, 0, false
	}
	first, last := w.samples[0], w.samples[len(w.samples)-1]
	periods := last.Periods - first.Periods
	if periods < MinPeriods {
		return 0, 0, false
	}
	ratio := float64(last.Throttled-first.Throttled) / float64(periods)
	if ratio > 1 {
		ratio = 1
	}
	percent = int32(ratio*100/Step+0.5) * Step
	return percent, last.at.Sub(first.at), true
}
