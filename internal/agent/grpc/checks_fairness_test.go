package grpc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
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

// The wait of the cooldown reaches the caller as a number: a RetryInfo detail on the status, and ItemResult.retry_after (with the machine
// reason COOLDOWN and retryable) on the item. The text stays for people.
func TestCooldownRefusalCarriesTheWaitAsANumber(t *testing.T) {
	err := cooldownError(6400 * time.Millisecond)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted || !strings.Contains(st.Message(), "wait 7 seconds") {
		t.Fatalf("status %v %q", st.Code(), st.Message())
	}
	var delay time.Duration
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			delay = ri.GetRetryDelay().AsDuration()
		}
	}
	if delay != 7*time.Second {
		t.Fatalf("RetryInfo delay %v, want 7s (a whole second above the exact wait, never below it)", delay)
	}
	ref := &protobuf.ItemRef{Name: "l"}
	res := failedResult(ref, err)
	if res.State != protobuf.ItemState_ITEM_STATE_FAILED || res.Reason != client.ReasonCooldown || !res.Retryable || res.RetryAfter.AsDuration() != 7*time.Second || !strings.Contains(res.Error, "wait 7 seconds") {
		t.Fatalf("%+v", res)
	}
	// the number never falls below the exact wait, even for a fraction of a second
	if got := failedResult(ref, cooldownError(200*time.Millisecond)).RetryAfter.AsDuration(); got < 200*time.Millisecond || got != time.Second {
		t.Fatalf("a short wait: %v", got)
	}
	// other failures carry no retry_after, and are not turned into a cooldown
	for _, other := range []error{errors.New("x"), status.Error(codes.ResourceExhausted, "busy"), notReadyError{group: "g"}} {
		if r := failedResult(ref, other); r.RetryAfter != nil || r.Reason == client.ReasonCooldown {
			t.Errorf("%v: %+v", other, r)
		}
	}
}
