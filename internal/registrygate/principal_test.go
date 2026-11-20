package registrygate

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	f.auth = &Authenticator{Reviewer: f.rev, Accounts: testAccounts, Schedule: f.sch, BindSource: true, Now: func() time.Time { return *f.clock }}
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
	a := &Authenticator{Reviewer: &countingReviewer{}, Accounts: testAccounts, Schedule: NewSchedule()}
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
	// a token the API server refuses: remembered for 5 seconds
	f.auth.Reviewer = reviewerFunc(func(string, []string) Review { rev.calls++; return Review{} })
	before := rev.calls
	for i := 0; i < 3; i++ {
		_, _ = f.authenticate("bad", "x:1")
	}
	if rev.calls != before+1 {
		t.Fatalf("a refused token is asked about once: %d", rev.calls-before)
	}
	*f.clock = f.clock.Add(6 * time.Second)
	_, _ = f.authenticate("bad", "x:1")
	if rev.calls != before+2 {
		t.Fatalf("after 5 seconds it is asked again: %d", rev.calls-before)
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
