package imagecache

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// What the components of the laboratory present to the registry gate: a projected ServiceAccount token of the audience TokenAudience in
// the header HeaderNodeToken. The gate reviews it with the API server and learns who calls (the relay of a node, the operator, the agent);
// it trusts no other X-Cib-* header, and a component removes every one of them from what it relays (StripPlatformHeaders).
const (
	// HeaderNodeToken carries the token.
	HeaderNodeToken = "X-Cib-Node-Token"
	// HeaderPrefix is the prefix of every header the platform defines on the way to the gate.
	HeaderPrefix = "X-Cib-"
	// TokenAudience is the audience of the projected token the gate accepts.
	TokenAudience = "laboratory-registry-gate"
	// DefaultTokenPath is where the chart projects the token into the pod.
	DefaultTokenPath = "/var/run/secrets/registry-gate/token"
	// TokenReread is how often the token file is read again (the kubelet rotates a projected token well before it expires).
	TokenReread = 30 * time.Second
)

// TokenFile is a projected token read from a file and read again every TokenReread.
type TokenFile struct {
	Path string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	token string
	at    time.Time
	err   error
}

// NewTokenFile returns a TokenFile of path.
func NewTokenFile(path string) *TokenFile { return &TokenFile{Path: path} }

// Token returns the current token. A file that cannot be read is an error until a read succeeds, except that the last good token is
// kept for the time of one interval.
func (t *TokenFile) Token() (string, error) {
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.at.IsZero() && now().Sub(t.at) < TokenReread {
		return t.token, t.err
	}
	raw, err := os.ReadFile(t.Path)
	t.at = now()
	if err != nil {
		t.token, t.err = "", err
		return "", err
	}
	t.token, t.err = strings.TrimSpace(string(raw)), nil
	return t.token, nil
}

// StripPlatformHeaders removes every X-Cib-* header: nothing a client sends may be taken for what the platform says.
func StripPlatformHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), HeaderPrefix) {
			delete(h, k)
		}
	}
}

// SetNodeToken makes h carry the token of t as its only X-Cib-* header.
func (t *TokenFile) SetNodeToken(h http.Header) error {
	StripPlatformHeaders(h)
	tok, err := t.Token()
	if err != nil {
		return err
	}
	h.Set(HeaderNodeToken, tok)
	return nil
}

// RoundTripper gives every request the token of t (and no other X-Cib-* header) before it goes to next (nil: http.DefaultTransport).
func (t *TokenFile) RoundTripper(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return tokenTransport{t: t, next: next}
}

type tokenTransport struct {
	t    *TokenFile
	next http.RoundTripper
}

func (tt tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if err := tt.t.SetNodeToken(r.Header); err != nil {
		return nil, err
	}
	return tt.next.RoundTrip(r)
}
