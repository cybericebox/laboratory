package checker

import (
	"encoding/json"
	"sort"
)

// RunRequest is the value of the annotation laboratory.cybericebox.com/run-checks (names.AnnotationRunChecks): a token that
// grows by one with every request the agent accepts, and the ids to run. No ids means every check that allows a manual run.
type RunRequest struct {
	Token int64    `json:"token"`
	IDs   []string `json:"ids,omitempty"`
}

// ParseRunRequest reads the annotation; ok is false for an empty or damaged value (which is ignored, never guessed at).
func ParseRunRequest(raw string) (RunRequest, bool) {
	if raw == "" {
		return RunRequest{}, false
	}
	var r RunRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil || r.Token <= 0 {
		return RunRequest{}, false
	}
	return r, true
}

// requests merges the requests of one lab that the checker has seen but not yet taken. The agent already merges a request into one
// the checker has not acknowledged; this does the same on the checker's side for requests that overtake each other while a
// run is going (the informer may deliver token N+1 while N is still being worked on).
type requests struct {
	// taken is the highest token that was taken for work, or that the status already acknowledges.
	taken   int64
	pending *RunRequest
}

// offer notes a request. A token that is not newer than one already taken or pending is dropped. A newer one is merged into the
// pending request: the ids are united, and "all" (no ids) wins over a list.
func (q *requests) offer(r RunRequest) {
	if r.Token <= q.taken {
		return
	}
	if q.pending == nil {
		q.pending = &RunRequest{Token: r.Token, IDs: append([]string(nil), r.IDs...)}
		return
	}
	if r.Token <= q.pending.Token {
		return
	}
	merged := RunRequest{Token: r.Token}
	if len(q.pending.IDs) > 0 && len(r.IDs) > 0 {
		set := map[string]bool{}
		for _, id := range q.pending.IDs {
			set[id] = true
		}
		for _, id := range r.IDs {
			set[id] = true
		}
		for id := range set {
			merged.IDs = append(merged.IDs, id)
		}
		sort.Strings(merged.IDs)
	}
	q.pending = &merged
}

// take hands the pending request over for work, if any.
func (q *requests) take() (RunRequest, bool) {
	if q.pending == nil {
		return RunRequest{}, false
	}
	r := *q.pending
	q.pending = nil
	q.taken = r.Token
	return r, true
}

// startFrom notes the acknowledgement found in the status when a lab is first seen: a token up to it has been taken (by this checker
// before a restart, or by the checker of another node) and is not run again. It is NOT used on later updates of the status: when
// the lab has devices on several nodes, the checker that finishes first writes the acknowledgement (there is one number), and
// the others must still run their part of the request.
func (q *requests) startFrom(ack int64) { q.taken = max(q.taken, ack) }
