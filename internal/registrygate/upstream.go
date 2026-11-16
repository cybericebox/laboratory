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
}

// Creds are the credentials of a pull. Empty is anonymous.
type Creds struct{ User, Pass string }

func (c Creds) anonymous() bool { return c.User == "" && c.Pass == "" }

// Upstream calls the upstream registries: the token dance, the HEAD that proves access, the GET that fills.
type Upstream struct {
	HTTP *http.Client
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
// registry asks for one). Only a DIRECT 200 from the registry is Allowed. 401, 403 and a refused token are Denied, and so is a 404:
// GHCR, Docker Hub and others answer 404, not 401, for a private repository the caller cannot read, so on the proof a 404 cannot be
// told from a refusal (a 404 for an object fetched AFTER a valid proof is NotFound, see Fetch). 429, 5xx, a timeout, DNS and anything
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
	resp, err := u.do(ctx, http.MethodHead, reg, o, c, credKey)
	if errors.Is(err, errTokenRefused) {
		return Denied, nil, nil
	}
	if err != nil {
		return Unreachable, nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return Allowed, resp.Header.Clone(), nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return Denied, nil, nil
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
		if auth, err = u.answer(ctx, challenge, o.Repo, c, tokenKey); err != nil {
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
func (u *Upstream) answer(ctx context.Context, challenge, repo string, c Creds, tokenKey string) (string, error) {
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
		if err != nil || realm == "" || ru.Scheme != "https" && ru.Scheme != schemeHTTP {
			return "", fmt.Errorf("bad token realm %q", realm)
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
