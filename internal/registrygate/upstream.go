package registrygate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Registry is an upstream registry the gate may call: the name the lab's image carries and the URL the chart gives it. The URL comes
// only from the list, never from a request, so no host of a tenant's choosing is ever called.
type Registry struct {
	Name string
	URL  *url.URL
	// AuthHosts are the hosts besides the registry's own whose token endpoint this registry's challenge may name (Docker Hub:
	// auth.docker.io). A realm anywhere else is refused: the credentials of a pull are never sent to a host the entry does not name.
	AuthHosts []string
	// Redirects are the host patterns (normalized by ParseRedirectPatterns) a MANIFEST request to this registry may be redirected to
	// (registry.k8s.io answers every manifest with a 307 to a regional Artifact Registry host). Empty: no redirect is followed, as always.
	Redirects []string
}

// realmAllowed says whether a token realm may receive the pull's credentials: https, on the registry's own host or one of its AuthHosts;
// plain http only for a registry that is itself plain http (inside the cluster, the tests), on its own host.
func (r Registry) realmAllowed(ru *url.URL) bool {
	host := ru.Hostname()
	if host == "" {
		return false
	}
	switch ru.Scheme {
	case "https":
	case schemeHTTP:
		return r.URL.Scheme == schemeHTTP && ru.Host == r.URL.Host
	default:
		return false
	}
	if host == r.URL.Hostname() {
		return true
	}
	for _, h := range r.AuthHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// Creds are the credentials of a pull. Empty is anonymous.
type Creds struct{ User, Pass string }

func (c Creds) anonymous() bool { return c.User == "" && c.Pass == "" }

// Upstream calls the upstream registries: the token dance, the HEAD that proves access, the GET that fills.
type Upstream struct {
	HTTP *http.Client
	// Timeout is the one deadline of a manifest proof that follows redirects, the whole chain included (the header timeout of a direct
	// request). Zero: none besides the transport's.
	Timeout time.Duration
	// Now is the clock of the token cache (tests).
	Now func() time.Time

	mu     sync.Mutex
	tokens map[string]token
}

type token struct {
	value   string
	expires time.Time
}

// NewUpstream is an upstream client that never follows a redirect by itself: the gate decides what a redirect may carry. headerTimeout
// bounds the wait for the answer of a request; the body of a blob has no deadline of its own (it is as long as the layer is).
func NewUpstream(headerTimeout time.Duration) *Upstream {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = headerTimeout
	return &Upstream{
		Timeout: headerTimeout,
		HTTP: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Now:    time.Now,
		tokens: map[string]token{},
	}
}

// Object is one thing to fetch: the path under /v2 of the upstream, "<repo>/manifests/<ref>" or "<repo>/blobs/<digest>".
type Object struct {
	Repo   string
	Kind   string // manifests or blobs
	Ref    string
	Accept string // the client's Accept header (manifest media types)
	Range  string // the client's Range header, for a blob streamed through without being stored
}

func (o Object) path() string { return "/v2/" + o.Repo + "/" + o.Kind + "/" + o.Ref }

// Check is the proof of 5.2: the HEAD of the object with the pull's credentials (a token for repository:<repo>:pull first when the
// registry asks for one). Only a DIRECT 200 from the registry is Allowed (or, for a manifest of a registry with Redirects, the 200 at the end of redirects to the hosts that list allows, judged as a direct answer, credentials never sent to them). 401, 403 and a refused token are Denied. A 404 is NotFound:
// GHCR, Docker Hub and others answer 404, not 401, for a private repository the caller cannot read, but the same 404 answers a ref that
// is only missing, and the ref is the caller's choice, so the proof alone cannot tell them apart: the cache decides (a 404 on a ref that
// was already proven is a revocation; a 404 on any other ref is recorded for that ref only). 429, 5xx, a timeout, DNS and anything
// else are Unreachable. A redirect is never a proof (a registry may redirect without having looked at who asks, and a 200 of a CDN says
// nothing about the credentials): a redirected answer is inconclusive, Unreachable. A token alone is no proof either (Docker Hub gives
// an empty-scope token for a repository one cannot read): only the HEAD is. credKey names the credentials in the token cache (the HMAC
// hash, never the secret).
func (u *Upstream) Check(ctx context.Context, reg Registry, o Object, c Creds, credKey string) (Outcome, error) {
	out, _, err := u.Proof(ctx, reg, o, c, credKey)
	return out, err
}

// Proof is Check that also returns the headers of the answer (Docker-Content-Digest of a tag).
func (u *Upstream) Proof(ctx context.Context, reg Registry, o Object, c Creds, credKey string) (Outcome, http.Header, error) {
	follows := len(reg.Redirects) > 0 && o.Kind == kindManifests
	if follows && u.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.Timeout)
		defer cancel()
	}
	resp, err := u.do(ctx, http.MethodHead, reg, o, c, credKey)
	if errors.Is(err, errTokenRefused) {
		return Denied, nil, nil
	}
	if errors.Is(err, errBadRealm) {
		return BadRealm, nil, err
	}
	if err != nil {
		return Unreachable, nil, err
	}
	if follows {
		// Only for a registry that lists redirect hosts: a redirect to a listed host is followed and the final answer is judged as a direct one.
		var ok bool
		if resp, ok, err = u.followProof(ctx, reg, resp, http.MethodHead, o); err != nil {
			return Unreachable, nil, err
		} else if !ok {
			return Unreachable, nil, nil
		}
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return Allowed, resp.Header.Clone(), nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return Denied, nil, nil
	case http.StatusNotFound:
		return NotFound, nil, nil
	}
	return Unreachable, nil, nil // 3xx included: inconclusive
}

// Head is the HEAD of an object that is not a proof (the headers of a blob served after the decision was made): a redirect is followed
// without Authorization and only a 200 at the end is Allowed.
func (u *Upstream) Head(ctx context.Context, reg Registry, o Object, c Creds, credKey string) (Outcome, http.Header, error) {
	resp, err := u.do(ctx, http.MethodHead, reg, o, c, credKey)
	if errors.Is(err, errTokenRefused) {
		return Denied, nil, nil
	}
	if errors.Is(err, errBadRealm) {
		return BadRealm, nil, err
	}
	if err != nil {
		return Unreachable, nil, err
	}
	resp, redirected, err := u.follow(ctx, reg, resp, http.MethodHead)
	if err != nil {
		return Unreachable, nil, err
	}
	defer drain(resp)
	return classify(resp.StatusCode, redirected), resp.Header.Clone(), nil
}

// classify is the meaning of a final answer of a data transfer (never of a proof). After a redirect the answer is the CDN's, not the registry's: only a 200 counts, and a
// refusal there is an outage of the transport, not a denial of the pull.
func classify(code int, redirected bool) Outcome {
	switch {
	case code == http.StatusOK:
		return Allowed
	case redirected:
		return Unreachable
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return Denied
	case code == http.StatusNotFound:
		return NotFound
	}
	return Unreachable
}

// follow follows the redirects of a response (at most five) over https and WITHOUT the Authorization header; on plain http only
// inside the upstream's own host. It returns the last response and whether a redirect was followed.
func (u *Upstream) follow(ctx context.Context, reg Registry, resp *http.Response, method string) (*http.Response, bool, error) {
	redirected := false
	for hops := 0; resp.StatusCode >= 300 && resp.StatusCode < 400; hops++ {
		loc, lerr := resp.Location()
		drain(resp)
		if hops >= 5 {
			return nil, true, errors.New("too many upstream redirects")
		}
		if lerr != nil || loc.Scheme != "https" && !u.allowInsecure(reg, loc) {
			return nil, true, fmt.Errorf("upstream redirect refused: %v", lerr)
		}
		req, err := http.NewRequestWithContext(ctx, method, loc.String(), nil) // no Authorization
		if err != nil {
			return nil, true, err
		}
		if resp, err = u.HTTP.Do(req); err != nil {
			return nil, true, err
		}
		redirected = true
	}
	return resp, redirected, nil
}

// Fetch is the GET of the object with the pull's credentials. A redirect (a blob often goes to a CDN) is followed over https and
// WITHOUT the Authorization header: a transport detail, not a proof of anything. The caller closes the body. The outcome is Allowed
// with a response, or another outcome with none (the same meaning as for Check).
func (u *Upstream) Fetch(ctx context.Context, reg Registry, o Object, c Creds, credKey string) (*http.Response, Outcome, error) {
	resp, err := u.do(ctx, http.MethodGet, reg, o, c, credKey)
	if errors.Is(err, errTokenRefused) {
		return nil, Denied, nil
	}
	if errors.Is(err, errBadRealm) {
		return nil, BadRealm, err
	}
	if err != nil {
		return nil, Unreachable, err
	}
	resp, redirected, err := u.follow(ctx, reg, resp, http.MethodGet)
	if err != nil {
		return nil, Unreachable, err
	}
	if resp.StatusCode == http.StatusPartialContent && o.Range != "" {
		return resp, Allowed, nil
	}
	if out := classify(resp.StatusCode, redirected); out != Allowed {
		drain(resp)
		return nil, out, nil
	}
	return resp, Allowed, nil
}

// allowInsecure lets a redirect stay on plain http only inside the upstream's own host when that upstream is itself plain http (a
// registry of the same cluster, and the tests); a redirect to another host over http is refused.
func (u *Upstream) allowInsecure(reg Registry, loc *url.URL) bool {
	return reg.URL.Scheme == schemeHTTP && loc.Scheme == schemeHTTP && loc.Host == reg.URL.Host
}

// do sends the request, answers a challenge, and returns the response that is not a challenge.
func (u *Upstream) do(ctx context.Context, method string, reg Registry, o Object, c Creds, credKey string) (*http.Response, error) {
	target := reg.URL.JoinPath(o.path()).String()
	tokenKey := credKey + "|" + reg.Name + "|" + o.Repo
	auth := u.cachedAuth(tokenKey)
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, err
		}
		if o.Accept != "" {
			req.Header.Set("Accept", o.Accept)
		}
		if o.Range != "" {
			req.Header.Set("Range", o.Range)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := u.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return resp, nil
		}
		challenge := resp.Header.Get("Www-Authenticate")
		if challenge == "" {
			return resp, nil // a plain 401: a refusal, with nothing to answer
		}
		drain(resp)
		if auth, err = u.answer(ctx, reg, challenge, o.Repo, c, tokenKey); err != nil {
			return nil, err
		}
		if auth == "" { // nothing to answer with: the registry's 401 stands
			req.Header.Del("Authorization")
			return u.HTTP.Do(req)
		}
	}
	return nil, errors.New("unreachable")
}

func (u *Upstream) cachedAuth(key string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if t, ok := u.tokens[key]; ok {
		if u.Now().Before(t.expires) {
			return "Bearer " + t.value
		}
		delete(u.tokens, key)
	}
	return ""
}

// answer turns a WWW-Authenticate challenge into an Authorization value: a Bearer token fetched with the credentials (or none), or
// Basic with the credentials. An empty answer means there is nothing to send (anonymous, and the registry wants Basic).
func (u *Upstream) answer(ctx context.Context, reg Registry, challenge, repo string, c Creds, tokenKey string) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch scheme {
	case "basic":
		if c.anonymous() {
			return "", nil
		}
		return basic(c), nil
	case "bearer":
		realm := params["realm"]
		ru, err := url.Parse(realm)
		if err != nil || realm == "" || !reg.realmAllowed(ru) {
			return "", errBadRealm // the realm is not echoed: it is the upstream's word
		}
		q := ru.Query()
		if s := params["service"]; s != "" {
			q.Set("service", s)
		}
		q.Set("scope", "repository:"+repo+":pull")
		ru.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
		if err != nil {
			return "", err
		}
		if !c.anonymous() {
			req.SetBasicAuth(c.User, c.Pass)
		}
		resp, err := u.HTTP.Do(req)
		if err != nil {
			return "", err
		}
		defer drain(resp)
		switch {
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			return "", errTokenRefused
		case resp.StatusCode != http.StatusOK:
			return "", fmt.Errorf("token endpoint answered %d", resp.StatusCode)
		}
		var body struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			return "", fmt.Errorf("token response: %w", err)
		}
		tok := body.Token
		if tok == "" {
			tok = body.AccessToken
		}
		if tok == "" {
			return "", errTokenRefused
		}
		ttl := time.Duration(body.ExpiresIn) * time.Second
		if ttl <= 0 {
			ttl = 60 * time.Second
		}
		u.mu.Lock()
		u.tokens[tokenKey] = token{tok, u.Now().Add(ttl * 9 / 10)}
		u.mu.Unlock()
		return "Bearer " + tok, nil
	}
	return "", fmt.Errorf("unsupported challenge %q", challenge)
}

// errTokenRefused: the token endpoint refused the credentials, which is a denial of the pull, not an outage.
var errTokenRefused = errors.New("the token endpoint refused the credentials")

// errBadRealm: the challenge names a token endpoint this registry may not use; no credential was sent to it.
var errBadRealm = errors.New("the token realm of the challenge is not one this registry may use")

func basic(c Creds) string {
	r := &http.Request{Header: http.Header{}}
	r.SetBasicAuth(c.User, c.Pass)
	return r.Header.Get("Authorization")
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// parseChallenge reads `Bearer realm="...",service="...",scope="..."` (and `Basic realm="..."`).
func parseChallenge(h string) (scheme string, params map[string]string) {
	params = map[string]string{}
	h = strings.TrimSpace(h)
	sp := strings.IndexByte(h, ' ')
	if sp < 0 {
		return strings.ToLower(h), params
	}
	scheme, rest := strings.ToLower(h[:sp]), h[sp+1:]
	for rest != "" {
		rest = strings.TrimLeft(rest, ", ")
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				break
			}
			val, rest = rest[1:1+end], rest[end+2:]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			val, rest = strings.TrimSpace(rest[:end]), rest[end:]
		}
		params[key] = val
	}
	return scheme, params
}
