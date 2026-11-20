package registrygate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// nodeFixture is a view with one node, one node-agent pod bound to it, and an authenticator over it.
type nodeFixture struct {
	sch   *Schedule
	auth  *Authenticator
	rev   *countingReviewer
	clock *time.Time
}

func newNodeFixture(t *testing.T) *nodeFixture {
	t.Helper()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	f := &nodeFixture{sch: NewSchedule(), rev: &countingReviewer{}, clock: &now}
	f.sch.Now = func() time.Time { return *f.clock }
	f.sch.Confirm()
	f.sch.SetNode(NodeInfo{Name: "node-a", Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.5")}})
	f.sch.SetNode(NodeInfo{Name: "node-b", Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.6")}})
	f.sch.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-a", UID: "uid-a", Node: "node-a", Phase: "Running"})
	f.sch.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-b", UID: "uid-b", Node: "node-b", Phase: "Running"})
	f.auth = &Authenticator{Reviewer: f.rev, Accounts: testAccounts, Schedule: f.sch, BindSource: true, Precheck: anyShape, Now: func() time.Time { return *f.clock }}
	return f
}

func (f *nodeFixture) authenticate(token, remote string) (Principal, error) {
	req := httptest.NewRequest(http.MethodGet, "http://x/v2/", nil)
	req.RemoteAddr = remote
	req.Header.Set(HeaderNodeToken, token)
	return f.auth.Authenticate(context.Background(), req)
}

func TestANodeTokenNamesItsNodeFromTheGatesOwnViewOfThePod(t *testing.T) {
	f := newNodeFixture(t)
	p, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:41000")
	if err != nil || p.Kind != KindNode || p.Node != "node-a" || p.Pod != "agent-a" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = f.authenticate("node-token-agent-b/uid-b", "10.0.0.6:1")
	if err != nil || p.Node != "node-b" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestANodeTokenIsRefusedWhenThePodDoesNotCheckOut(t *testing.T) {
	f := newNodeFixture(t)
	for name, c := range map[string]struct{ token, remote string }{
		"the UID of the pod differs":      {"node-token-agent-a/uid-forged", "10.0.0.5:1"},
		"the pod is unknown":              {"node-token-agent-zz/uid-zz", "10.0.0.5:1"},
		"a token not bound to a pod":      {"agent-token-but-as-node", "10.0.0.5:1"},
		"the peer is another node":        {"node-token-agent-a/uid-a", "10.0.0.6:1"},
		"the peer is no node at all":      {"node-token-agent-a/uid-a", "192.0.2.9:1"},
		"the peer is not an address":      {"node-token-agent-a/uid-a", "garbage"},
		"the pod is in another namespace": {"", ""},
	} {
		if c.token == "" {
			continue
		}
		if _, err := f.authenticate(c.token, c.remote); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a pod that is being deleted no longer speaks for a node
	f.sch.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-a", UID: "uid-a", Node: "node-a", Terminating: true})
	if _, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:1"); err == nil {
		t.Error("a terminating pod is accepted")
	}
	// a pod not bound to a node
	f.sch.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-c", UID: "uid-c"})
	if _, err := f.authenticate("node-token-agent-c/uid-c", "10.0.0.5:1"); err == nil {
		t.Error("a pod without a node is accepted")
	}
}

// A pod of the same name in another namespace is not the node-agent: the namespace is the account's.
func TestTheNodeAgentPodIsLookedUpInTheNamespaceOfItsAccount(t *testing.T) {
	f := newNodeFixture(t)
	f.sch.DeletePod("laboratory-system", "agent-a")
	f.sch.SetPod(PodInfo{Namespace: "attacker", Name: "agent-a", UID: "uid-a", Node: "node-a"})
	if _, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:1"); err == nil {
		t.Fatal("a pod of the same name elsewhere must not stand for the node-agent")
	}
}

// When the token also names its node, it must be the node of the pod.
func TestAnExtraNodeNameMustAgree(t *testing.T) {
	f := newNodeFixture(t)
	f.auth.Reviewer = reviewerFunc(func(token string, aud []string) Review {
		extra := map[string][]string{extraPodName: {"agent-a"}, extraPodUID: {"uid-a"}}
		if token == "agrees" {
			extra[extraNodeName] = []string{"node-a"}
		} else {
			extra[extraNodeName] = []string{"node-b"}
		}
		return Review{Authenticated: true, Username: "system:serviceaccount:laboratory-system:laboratory-node-agent", Audiences: aud, Extra: extra}
	})
	if _, err := f.authenticate("agrees", "10.0.0.5:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.authenticate("disagrees", "10.0.0.5:1"); err == nil {
		t.Fatal("a node name that is not the pod's node must be refused")
	}
}

type reviewerFunc func(token string, aud []string) Review

func (f reviewerFunc) Review(_ context.Context, token string, aud []string) (Review, error) {
	return f(token, aud), nil
}

// Source binding is off when asked: the peer address plays no part then.
func TestSourceBindingCanBeSwitchedOff(t *testing.T) {
	f := newNodeFixture(t)
	f.auth.BindSource = false
	if _, err := f.authenticate("node-token-agent-a/uid-a", "192.0.2.9:1"); err != nil {
		t.Fatal(err)
	}
}

// The view is too old: nothing that needs it is answered (503), not even a token that is cached.
func TestAStaleViewAnswersUnavailable(t *testing.T) {
	f := newNodeFixture(t)
	if _, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:1"); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(14 * time.Minute)
	if _, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:1"); err != nil {
		t.Fatalf("14 minutes is within the 15 of the default: %v", err)
	}
	*f.clock = f.clock.Add(2 * time.Minute) // 16 minutes since the last contact
	if _, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:1"); err != ErrUnavailable {
		t.Fatalf("a view older than the limit: %v", err)
	}
	f.sch.Confirm() // the API server is back
	if _, err := f.authenticate("node-token-agent-a/uid-a", "10.0.0.5:1"); err != nil {
		t.Fatalf("after a contact: %v", err)
	}
	// the operator and the agent do not need the view to be known
	*f.clock = f.clock.Add(time.Hour)
	if p, err := f.authenticate("operator-token", "192.0.2.1:1"); err != nil || p.Kind != KindOperator {
		t.Fatalf("operator: %+v %v", p, err)
	}
}

func TestAViewThatNeverSyncedIsNotFresh(t *testing.T) {
	s := NewSchedule()
	if s.Fresh(time.Hour) {
		t.Fatal("never confirmed")
	}
	s.Confirm()
	if !s.Fresh(time.Hour) {
		t.Fatal("confirmed")
	}
	a := &Authenticator{Reviewer: &countingReviewer{}, Accounts: testAccounts, Schedule: NewSchedule(), Precheck: anyShape}
	req := httptest.NewRequest(http.MethodGet, "http://x/v2/", nil)
	req.Header.Set(HeaderNodeToken, "node-token-p/u")
	if _, err := a.Authenticate(context.Background(), req); err != ErrUnavailable {
		t.Fatalf("a node token before the view has synced: %v", err)
	}
}

// The review is cached for min(exp of the token, the cache time); a refusal is remembered for a few seconds only.
func TestTokenCacheLifetime(t *testing.T) {
	f := newNodeFixture(t)
	f.auth.BindSource = false
	jwt := func(exp time.Time) string {
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
		return "h." + payload + ".s"
	}
	rev := f.auth.Reviewer.(*countingReviewer)
	short := jwt(f.clock.Add(time.Minute))
	f.auth.Reviewer = reviewerFunc(func(token string, aud []string) Review {
		rev.calls++
		return Review{Authenticated: true, Username: "system:serviceaccount:laboratory-agent:laboratory-agent", Audiences: aud}
	})
	for i := 0; i < 3; i++ {
		if _, err := f.authenticate(short, "x:1"); err != nil {
			t.Fatal(err)
		}
	}
	if rev.calls != 1 {
		t.Fatalf("cached: %d reviews", rev.calls)
	}
	*f.clock = f.clock.Add(61 * time.Second) // past the token's own expiry, inside the 5 minutes
	if _, err := f.authenticate(short, "x:1"); err != nil || rev.calls != 2 {
		t.Fatalf("the token's expiry shortens the cache: %d reviews, %v", rev.calls, err)
	}
	// a token the API server refuses: remembered for 30 seconds
	f.auth.Reviewer = reviewerFunc(func(string, []string) Review { rev.calls++; return Review{} })
	before := rev.calls
	for i := 0; i < 3; i++ {
		_, _ = f.authenticate("bad", "x:1")
	}
	if rev.calls != before+1 {
		t.Fatalf("a refused token is asked about once: %d", rev.calls-before)
	}
	*f.clock = f.clock.Add(31 * time.Second)
	_, _ = f.authenticate("bad", "x:1")
	if rev.calls != before+2 {
		t.Fatalf("after 30 seconds it is asked again: %d", rev.calls-before)
	}
}

// A token for another audience is not accepted even if the API server calls it authenticated.
func TestAnotherAudienceIsRefused(t *testing.T) {
	f := newNodeFixture(t)
	f.auth.Reviewer = reviewerFunc(func(string, []string) Review {
		return Review{Authenticated: true, Username: "system:serviceaccount:laboratory-agent:laboratory-agent", Audiences: []string{"https://kubernetes.default.svc"}}
	})
	if _, err := f.authenticate("t", "x:1"); err != ErrUnauthenticated {
		t.Fatalf("%v", err)
	}
}

func TestStripPlatformHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Cib-Node-Token", "t")
	h.Set("x-cib-anything", "1")
	h.Set("X-Cibx", "kept") // not the prefix "X-Cib-"
	h.Set("Accept", "x")
	StripPlatformHeaders(h)
	if h.Get("X-Cib-Node-Token") != "" || h.Get("X-Cib-Anything") != "" || h.Get("X-Cibx") == "" || h.Get("Accept") == "" {
		t.Fatalf("%v", h)
	}
}

// anyShape is the precheck of the tests that use opaque token names; the real one is tested below.
func anyShape(string, time.Time) bool { return true }

const nodeSub = "system:serviceaccount:laboratory-system:laboratory-node-agent"

func jwt(exp time.Time, sub string, aud any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "RS256"}) + "." + enc(map[string]any{"exp": exp.Unix(), "sub": sub, "aud": aud}) + ".c2ln"
}

// Review finding 4 / 2.4.6a: what cannot be one of ours never reaches the API server; what can is bounded per address and in flight, and one
// token is reviewed once however many requests carry it.
func TestTokenReviewFloodIsBoundedBeforeTheAPIServer(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	rev := &countingReviewer{}
	a := &Authenticator{Reviewer: rev, Accounts: testAccounts, Schedule: NewSchedule(), Now: func() time.Time { return now }}
	ask := func(tok, remote string) error {
		r := httptest.NewRequest("GET", "http://x/v2/", nil)
		r.RemoteAddr = remote
		r.Header.Set(HeaderNodeToken, tok)
		_, err := a.Authenticate(context.Background(), r)
		return err
	}
	aud := []string{TokenAudience}
	for name, tok := range map[string]string{
		"not a jwt":          "garbage",
		"two parts":          "a.b",
		"payload not base64": "a.!!.c",
		"expired":            jwt(now.Add(-time.Minute), nodeSub, aud),
		"another subject":    jwt(now.Add(time.Hour), "system:serviceaccount:default:somebody", aud),
		"no subject":         jwt(now.Add(time.Hour), "", aud),
		"another audience":   jwt(now.Add(time.Hour), nodeSub, []string{"other"}),
		"no audience":        jwt(now.Add(time.Hour), nodeSub, nil),
		"huge":               strings.Repeat("a", 9<<10) + ".b.c",
	} {
		if err := ask(tok, "192.0.2.1:1"); err != ErrUnauthenticated {
			t.Errorf("%s: %v", name, err)
		}
	}
	if rev.calls != 0 {
		t.Fatalf("the API server was called %d times for tokens that cannot be ours", rev.calls)
	}
	// a token of the right shape is reviewed; a refused one is remembered for 30 seconds, so one bad token costs one API call
	bad := jwt(now.Add(time.Hour), nodeSub, aud)
	for i := 0; i < 5; i++ {
		if err := ask(bad, "192.0.2.7:1"); err != ErrUnauthenticated {
			t.Fatal(err)
		}
	}
	if rev.calls != 1 {
		t.Fatalf("one bad token, %d API calls", rev.calls)
	}
	now = now.Add(31 * time.Second)
	_ = ask(bad, "192.0.2.7:1")
	if rev.calls != 2 {
		t.Fatalf("after the negative cache lapsed: %d", rev.calls)
	}
	// per address: the burst, then refused; another address and the passing of time each have room
	a.ReviewBurst, a.ReviewRate = 3, 1
	rev.calls = 0
	var limited bool
	for i := 0; i < 10; i++ {
		tok := jwt(now.Add(time.Hour+time.Duration(i)*time.Second), nodeSub, aud)
		if ask(tok, "192.0.2.3:1") == ErrRateLimited {
			limited = true
		}
	}
	if !limited || rev.calls > 3 {
		t.Fatalf("limited=%v calls=%d: one address may cost the API server only its burst", limited, rev.calls)
	}
	if err := ask(jwt(now.Add(5*time.Hour), nodeSub, aud), "192.0.2.4:1"); errors.Is(err, ErrRateLimited) {
		t.Fatal("another address is not limited")
	}
	now = now.Add(5 * time.Second)
	if err := ask(jwt(now.Add(6*time.Hour), nodeSub, aud), "192.0.2.3:1"); errors.Is(err, ErrRateLimited) {
		t.Fatal("the bucket refills")
	}
}

type slowReviewer struct {
	calls   atomic.Int32
	release chan struct{}
}

func (s *slowReviewer) Review(ctx context.Context, token string, aud []string) (Review, error) {
	s.calls.Add(1)
	<-s.release
	return testReviewer{}.Review(ctx, "agent-token", aud)
}

func TestOneTokenIsReviewedOnceAndInFlightReviewsAreCapped(t *testing.T) {
	rev := &slowReviewer{release: make(chan struct{})}
	a := &Authenticator{Reviewer: rev, Accounts: testAccounts, Schedule: NewSchedule(), Precheck: anyShape, MaxReviews: 2, ReviewWait: 80 * time.Millisecond, ReviewBurst: 1000}
	run := func(tok string, errs chan<- error) {
		r := httptest.NewRequest("GET", "http://x/v2/", nil)
		r.Header.Set(HeaderNodeToken, tok)
		_, err := a.Authenticate(context.Background(), r)
		errs <- err
	}
	errs := make(chan error, 10)
	for i := 0; i < 5; i++ {
		go run("same", errs)
	}
	waitFor(t, func() bool { return rev.calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if rev.calls.Load() != 1 {
		t.Fatalf("five requests with one token made %d reviews", rev.calls.Load())
	}
	go run("second", errs)
	waitFor(t, func() bool { return rev.calls.Load() == 2 })
	// the third distinct token waits for a slot, then gives up: 503, no API call
	r := httptest.NewRequest("GET", "http://x/v2/", nil)
	r.Header.Set(HeaderNodeToken, "third")
	start := time.Now()
	if _, err := a.Authenticate(context.Background(), r); err != ErrUnavailable || rev.calls.Load() != 2 {
		t.Fatalf("over the cap: %v (%d calls)", err, rev.calls.Load())
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("it should have waited for a slot first")
	}
	close(rev.release)
	for i := 0; i < 6; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
