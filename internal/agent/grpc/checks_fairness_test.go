package grpc

import (
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
)

func TestCooldownWait(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for name, c := range map[string]struct {
		cool time.Duration
		prev int64
		want time.Duration
	}{
		"no cooldown":             {0, now.Unix() - 1, 0},
		"no earlier request":      {10 * time.Second, 0, 0},
		"just after":              {10 * time.Second, now.Unix(), 10 * time.Second},
		"part of the way":         {10 * time.Second, now.Unix() - 4, 6 * time.Second},
		"exactly at the cooldown": {10 * time.Second, now.Unix() - 10, 0},
		"long ago":                {10 * time.Second, now.Unix() - 3600, 0},
		"a clock that went back":  {10 * time.Second, now.Unix() + 3, 13 * time.Second},
	} {
		if got := cooldownWait(c.cool, c.prev, now); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestTimeLimitCeilingIsEnforcedWhereChecksAreAccepted(t *testing.T) {
	set := &checkSet{checks: []labchecks.Check{{ID: "slow", TimeLimitSeconds: 300}, {ID: "quick", TimeLimitSeconds: 10}}}
	h := &Handler{}
	h.features.CheckMaxTimeLimit = 120
	if err := h.requireChecks(set); err == nil {
		t.Fatal("a check above the ceiling was accepted")
	}
	h.features.CheckMaxTimeLimit = 300
	if err := h.requireChecks(set); err != nil {
		t.Fatalf("a check at the ceiling: %v", err)
	}
	h.features.CheckMaxTimeLimit = 0
	if err := h.requireChecks(set); err != nil {
		t.Fatalf("no ceiling: %v", err)
	}
	h.features.CheckMaxTimeLimit = 90
	if got := h.maxCheckTimeLimit(); got != 90 {
		t.Fatalf("reported %d", got)
	}
	h.features.CheckMaxTimeLimit = 100000
	if got := h.maxCheckTimeLimit(); got != labchecks.MaxCheckTimeLimit {
		t.Fatalf("a ceiling above the contract's: %d", got)
	}
}
