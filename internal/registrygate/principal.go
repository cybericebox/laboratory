package registrygate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	// HeaderNodeToken carries the projected ServiceAccount token of the caller (the relay of a node, the operator, the agent).
	HeaderNodeToken = "X-Cib-Node-Token"
	// HeaderPrefix is the prefix of every header the platform defines on the way to the gate. The gate trusts none of them except the
	// token header, and removes all of them before a request goes on to zot.
	HeaderPrefix = "X-Cib-"
	// TokenAudience is the audience of the projected tokens the gate accepts.
	TokenAudience = "laboratory-registry-gate"

	// The keys a bound ServiceAccount token carries in the TokenReview "extra" (the pod it is bound to, and, on newer Kubernetes, its node).
	extraPodName  = "authentication.kubernetes.io/pod-name"
	extraPodUID   = "authentication.kubernetes.io/pod-uid"
	extraNodeName = "authentication.kubernetes.io/node-name"
)

// Kind says what a caller is.
type Kind int

const (
	// KindNode is the node-agent of a lab node (its relay): containerd, the kubelet pulls and node-state.
	KindNode Kind = iota + 1
	// KindOperator is the operator.
	KindOperator
	// KindAgent is the management agent (the prewarm).
	KindAgent
)

func (k Kind) String() string {
	switch k {
	case KindNode:
		return "node"
	case KindOperator:
		return "operator"
	case KindAgent:
		return "agent"
	}
	return "unknown"
}

// Account is a ServiceAccount.
type Account struct{ Namespace, Name string }

func (a Account) username() string { return "system:serviceaccount:" + a.Namespace + ":" + a.Name }

// Accounts are the only ServiceAccounts the gate accepts.
type Accounts struct{ Node, Operator, Agent Account }

func (a Accounts) kindOf(username string) (Kind, Account, bool) {
	for _, c := range []struct {
		k   Kind
		acc Account
	}{{KindNode, a.Node}, {KindOperator, a.Operator}, {KindAgent, a.Agent}} {
		if c.acc.Name != "" && c.acc.username() == username {
			return c.k, c.acc, true
		}
	}
	return 0, Account{}, false
}

// Principal is an authenticated caller. For a node it carries the node the token's pod is bound to, taken from the gate's own view of
// the pod and never from what the caller says.
type Principal struct {
	Kind    Kind
	Account Account
	Node    string
	Pod     string
}

// Review is the answer of a TokenReview.
type Review struct {
	Authenticated bool
	Username      string
	Audiences     []string
	Extra         map[string][]string
	Error         string
}

// TokenReviewer verifies a token with the API server (spec.audiences is what the gate accepts).
type TokenReviewer interface {
	Review(ctx context.Context, token string, audiences []string) (Review, error)
}

// The ways authentication fails; a handler maps them to 401, 403 and 503.
var (
	// ErrUnauthenticated: no token, or the API server did not accept it.
	ErrUnauthenticated = errors.New("no valid token")
	// ErrForbidden: a valid token of an account the gate does not serve, or a node the gate cannot place.
	ErrForbidden = errors.New("the caller is not accepted")
	// ErrUnavailable: the answer would need the API server (or a view of it) and it is not reachable or is too old.
	ErrUnavailable = errors.New("the cluster view is not available")
	// ErrRateLimited: this source address has caused too many token reviews.
	ErrRateLimited = errors.New("too many token reviews from this address")
)

type reviewed struct {
	r       Review
	expires time.Time
}

// Authenticator turns a request into a Principal.
type Authenticator struct {
	Reviewer TokenReviewer
	Accounts Accounts
	Schedule *Schedule
	// CacheTTL is the longest a verified token is remembered (default 5 minutes, GATE_TOKEN_CACHE_TTL); the token's own expiry shortens it.
	CacheTTL time.Duration
	// MaxStale is the age of the cluster view past which nothing that needs it is answered (default 15 minutes, GATE_API_STALE_MAX).
	MaxStale time.Duration
	// BindSource requires the peer address of a node to be one of the node's addresses (GATE_BIND_SOURCE).
	BindSource bool
	Now        func() time.Time
	// Precheck decides from the token alone, before any API call, whether it can possibly be one of ours; nil: the structural check
	// (a JWT whose exp has not passed and whose audience is the gate's). Tests that use opaque tokens replace it.
	Precheck func(token string, now time.Time) bool
	// MaxReviews is how many TokenReview calls may be in flight at once (default 16); more are answered 503.
	MaxReviews int
	// ReviewRate and ReviewBurst bound the API calls one source address may cause (default 5 per second, burst 20); more are refused.
	ReviewRate  float64
	ReviewBurst int

	mu     sync.Mutex
	cache  map[[32]byte]reviewed
	bad    map[[32]byte]time.Time
	flight map[[32]byte]*reviewCall
	inUse  int
	rate   map[string]*bucket
}

type reviewCall struct {
	done chan struct{}
	rv   Review
	err  error
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

const maxCachedTokens = 20000

// Authenticate reads the token header and decides who the caller is. The review is cached by the hash of the token; the pod and the node
// of a node token are looked up in the view on every request, because a pod can end and a node can change under a token that is still valid.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	token := r.Header.Get(HeaderNodeToken)
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	rv, err := a.review(ctx, token, r.RemoteAddr)
	if err != nil {
		return Principal{}, err
	}
	kind, acc, ok := a.Accounts.kindOf(rv.Username)
	if !ok {
		return Principal{}, ErrForbidden
	}
	p := Principal{Kind: kind, Account: acc}
	if kind != KindNode {
		return p, nil
	}
	return a.placeNode(p, rv, r)
}

func (a *Authenticator) review(ctx context.Context, token, remote string) (Review, error) {
	h := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if c, ok := a.cache[h]; ok && now.Before(c.expires) {
		a.mu.Unlock()
		return c.r, nil
	}
	if at, ok := a.bad[h]; ok && now.Sub(at) < 5*time.Second {
		a.mu.Unlock()
		return Review{}, ErrUnauthenticated
	}
	a.mu.Unlock()
	// nothing below costs an API call until the token has the shape of one of ours
	pre := a.Precheck
	if pre == nil {
		pre = structurallyOurs
	}
	if !pre(token, now) {
		return Review{}, ErrUnauthenticated
	}
	if !a.allowSource(remote, now) {
		return Review{}, ErrRateLimited
	}
	call, leader, ok := a.join(h)
	if !ok {
		return Review{}, ErrUnavailable // too many reviews in flight
	}
	if !leader { // the same token is already being reviewed: share the answer
		select {
		case <-call.done:
			return call.rv, call.err
		case <-ctx.Done():
			return Review{}, ErrUnavailable
		}
	}
	rv, err := a.doReview(ctx, h, token, now)
	a.mu.Lock()
	delete(a.flight, h)
	a.inUse--
	a.mu.Unlock()
	call.rv, call.err = rv, err
	close(call.done)
	return rv, err
}

// join registers a review of the token h: the first caller leads (and must call the API), later callers share. ok is false when the
// leader cannot start because too many reviews are in flight.
func (a *Authenticator) join(h [32]byte) (call *reviewCall, leader, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, running := a.flight[h]; running {
		return c, false, true
	}
	limit := a.MaxReviews
	if limit <= 0 {
		limit = 16
	}
	if a.inUse >= limit {
		return nil, false, false
	}
	if a.flight == nil {
		a.flight = map[[32]byte]*reviewCall{}
	}
	a.inUse++
	c := &reviewCall{done: make(chan struct{})}
	a.flight[h] = c
	return c, true, true
}

func (a *Authenticator) doReview(ctx context.Context, h [32]byte, token string, now time.Time) (Review, error) {
	rv, err := a.Reviewer.Review(ctx, token, []string{TokenAudience})
	if err != nil {
		return Review{}, ErrUnavailable // a token that is not cached needs the API server
	}
	if !rv.Authenticated || !hasAudience(rv.Audiences, TokenAudience) {
		a.mu.Lock()
		if a.bad == nil {
			a.bad = map[[32]byte]time.Time{}
		}
		if len(a.bad) >= maxCachedTokens {
			for k, at := range a.bad { // drop what has lapsed; a full table of live entries is left as it is (a refusal is cheap)
				if now.Sub(at) >= 5*time.Second {
					delete(a.bad, k)
				}
			}
		}
		if len(a.bad) < maxCachedTokens {
			a.bad[h] = now
		}
		a.mu.Unlock()
		return Review{}, ErrUnauthenticated
	}
	ttl := a.CacheTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	expires := now.Add(ttl)
	if exp, ok := tokenExpiry(token); ok && exp.Before(expires) {
		expires = exp
	}
	a.mu.Lock()
	if a.cache == nil || len(a.cache) > maxCachedTokens {
		a.cache = map[[32]byte]reviewed{}
	}
	a.cache[h] = reviewed{r: rv, expires: expires}
	a.mu.Unlock()
	return rv, nil
}

// allowSource is a token bucket per source address on the calls that reach the API server.
func (a *Authenticator) allowSource(remote string, now time.Time) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	rate, burst := a.ReviewRate, float64(a.ReviewBurst)
	if rate <= 0 {
		rate = 5
	}
	if burst <= 0 {
		burst = 20
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rate == nil || len(a.rate) > maxCachedTokens {
		a.rate = map[string]*bucket{}
	}
	b := a.rate[host]
	if b == nil {
		b = &bucket{tokens: burst, at: now}
		a.rate[host] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// structurallyOurs is what can be told of a token without the API server: a JWT with a payload that names an expiry still ahead and the
// gate's audience, and an issuer. A token that fails this is refused without a TokenReview.
func structurallyOurs(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" || len(token) > 16<<10 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		Exp int64           `json:"exp"`
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp <= now.Unix() || c.Iss == "" {
		return false
	}
	var one string
	var many []string
	if json.Unmarshal(c.Aud, &one) == nil {
		return one == TokenAudience
	}
	if json.Unmarshal(c.Aud, &many) == nil {
		return hasAudience(many, TokenAudience)
	}
	return false
}

func hasAudience(have []string, want string) bool {
	for _, a := range have {
		if a == want {
			return true
		}
	}
	return false
}

// tokenExpiry reads the exp claim of a token that the API server has just accepted (so the claim is the signed one). It only ever
// shortens the cache time.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0), true
}

func first(m map[string][]string, k string) string {
	if v := m[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// placeNode learns the node of a node token from the gate's own view of the pod the token is bound to: the pod must be in the account's
// namespace with the UID the token names, and bound to a node; if the token also names a node it must be the same one; and when BindSource
// is on the peer address must be an address of that node.
func (a *Authenticator) placeNode(p Principal, rv Review, r *http.Request) (Principal, error) {
	if a.Schedule == nil || !a.Schedule.Fresh(a.maxStale()) {
		return Principal{}, ErrUnavailable
	}
	podName, podUID := first(rv.Extra, extraPodName), first(rv.Extra, extraPodUID)
	if podName == "" || podUID == "" {
		return Principal{}, ErrForbidden // a token that is not bound to a pod cannot name a node
	}
	pod, ok := a.Schedule.Pod(p.Account.Namespace, podName)
	if !ok || pod.UID != podUID || pod.Node == "" || pod.Terminating {
		return Principal{}, ErrForbidden
	}
	if n := first(rv.Extra, extraNodeName); n != "" && n != pod.Node {
		return Principal{}, ErrForbidden
	}
	p.Node, p.Pod = pod.Node, pod.Name
	if a.BindSource {
		node, ok := a.Schedule.Node(p.Node)
		if !ok || !peerIsOneOf(r.RemoteAddr, node.Addrs) {
			return Principal{}, ErrForbidden
		}
	}
	return p, nil
}

func (a *Authenticator) maxStale() time.Duration {
	if a.MaxStale > 0 {
		return a.MaxStale
	}
	return 15 * time.Minute
}

func peerIsOneOf(remote string, addrs []netip.Addr) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, a := range addrs {
		if a.Unmap() == ip {
			return true
		}
	}
	return false
}

// StripPlatformHeaders removes every X-Cib-* header from a request: none of them may reach zot or influence a decision (the token header is
// read before this is called).
func StripPlatformHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), HeaderPrefix) {
			h.Del(k)
		}
	}
}
