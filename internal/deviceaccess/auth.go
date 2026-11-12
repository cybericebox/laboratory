package deviceaccess

import (
	"errors"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// Auth checks the signature, the timestamp and the nonce of an open request. Check is the one place where an open request is
// authenticated; it runs before anything else is done with the request.
type Auth struct {
	// Keys returns the keys that are accepted (the current one and one more while a key is rotated).
	Keys func() [][]byte
	// Window is how far the timestamp may be from the clock of the service, and how long a nonce is remembered.
	Window time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	seen  map[string]time.Time
	order []seenNonce
}

type seenNonce struct {
	nonce string
	at    time.Time
}

// maxNonces bounds the memory of the replay cache. A full cache refuses new requests: it fails closed.
const maxNonces = 1 << 18

var (
	errUnsigned = errors.New("the request is not signed with an accepted key")
	errStale    = errors.New("the timestamp of the request is outside the window")
	errReplay   = errors.New("the request was seen before")
	errFull     = errors.New("too many requests in the window")
)

// NewAuth returns an Auth with the default window of 30 seconds.
func NewAuth(keys func() [][]byte) *Auth {
	return &Auth{Keys: keys, Window: 30 * time.Second}
}

func (a *Auth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Check authenticates a request. The signature comes first; only a validly signed request touches the replay cache, so an unauthenticated
// caller cannot fill it.
func (a *Auth) Check(r wire.OpenRequest) error {
	if a == nil || a.Keys == nil {
		return errUnsigned
	}
	keys := a.Keys()
	if len(keys) == 0 || !wire.VerifySignature(keys, r) {
		return errUnsigned
	}
	now := a.now()
	at := time.Unix(r.Time, 0)
	if at.Before(now.Add(-a.Window)) || at.After(now.Add(a.Window)) {
		return errStale
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string]time.Time{}
	}
	// forget what is older than the window (the order is the order of arrival)
	cut := now.Add(-2 * a.Window)
	for len(a.order) > 0 && a.order[0].at.Before(cut) {
		delete(a.seen, a.order[0].nonce)
		a.order = a.order[1:]
	}
	if _, dup := a.seen[r.Nonce]; dup {
		return errReplay
	}
	if len(a.seen) >= maxNonces {
		return errFull
	}
	a.seen[r.Nonce] = now
	a.order = append(a.order, seenNonce{r.Nonce, now})
	return nil
}
