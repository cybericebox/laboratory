package deviceaccess

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The authentication of the cable stream between the device access services of two nodes. Only the device access pods hold the key (a
// Secret of their namespace that nothing else mounts; it is not the access key of a tenant and not the grant of a console). The dialer
// signs its open request with an HMAC-SHA256 over every field, a time and a nonce; the acceptor checks the MAC in constant time, the time
// within CableWindow of its own clock and that it has not seen the nonce; its reply is signed over the request's nonce, so the dialer knows
// it talks to a holder of the key and that the answer is for its request. Authentication is not authorization: an accepted request is still
// served only when the node-agent of the acceptor's node has given it the same line (cable, UID, lab, lab UID and both ends), so a holder
// of the key can reach nothing but the serial port of a cable that exists.

// CableWindow is how far the time of an open request may be from the acceptor's clock.
const CableWindow = 30 * time.Second

// minCableKey is the shortest key accepted (bytes of the file after trimming); maxNonces bounds the nonces remembered (a cable stream is
// opened once per line and reconnect, so this is far more than 500 lines reconnecting at once need).
const (
	minCableKey = 32
	maxNonces   = 20000
)

var (
	errCableKey    = errors.New("no usable cable key")
	errCableMAC    = errors.New("the cable request is not authentic")
	errCableTime   = errors.New("the cable request is outside the time window")
	errCableReplay = errors.New("the nonce of the cable request was seen before")
)

// CableKey reads the key from a file, again whenever the file changes (a rotated Secret reaches the pod without a restart).
type CableKey struct {
	path string

	mu    sync.Mutex
	key   []byte
	mtime time.Time
	size  int64
}

// NewCableKey returns the key of the file at path.
func NewCableKey(path string) *CableKey { return &CableKey{path: path} }

// StaticCableKey is a key held in memory (tests).
func StaticCableKey(key []byte) *CableKey { return &CableKey{key: append([]byte(nil), key...)} }

// Get returns the key, or an error when there is none or it is too short.
func (k *CableKey) Get() ([]byte, error) {
	if k == nil {
		return nil, errCableKey
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.path == "" {
		if len(k.key) < minCableKey {
			return nil, errCableKey
		}
		return k.key, nil
	}
	st, err := os.Stat(k.path)
	if err != nil {
		return nil, errCableKey
	}
	if k.key == nil || !st.ModTime().Equal(k.mtime) || st.Size() != k.size {
		raw, err := os.ReadFile(k.path)
		if err != nil {
			return nil, errCableKey
		}
		key := []byte(strings.TrimSpace(string(raw)))
		if len(key) < minCableKey {
			return nil, errCableKey
		}
		k.key, k.mtime, k.size = key, st.ModTime(), st.Size()
	}
	return k.key, nil
}

// field joins the fields of a MAC: a NUL never appears in a field (validateCableOpen refuses it), so the encoding is unambiguous.
func macOf(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		_, _ = m.Write([]byte(p))
		_, _ = m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

func openMAC(key []byte, r wire.CableOpen) string {
	return macOf(key, "cib-cable-open", strconv.Itoa(r.V), r.Namespace, r.Name, r.UID, r.Lab, r.LabUID,
		r.Serial.Device, r.Serial.Port, r.Console.Device, r.Console.Port, strconv.FormatInt(r.Time, 10), r.Nonce)
}

func replyMAC(key []byte, r wire.CableReply) string {
	return macOf(key, "cib-cable-reply", r.Nonce, strconv.FormatBool(r.OK), r.Error)
}

// signOpen fills the time, a fresh nonce and the MAC of an open request.
func signOpen(key []byte, r wire.CableOpen, now time.Time) (wire.CableOpen, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return r, err
	}
	r.V = wire.RelayVersion
	r.Time = now.UnixMilli()
	r.Nonce = hex.EncodeToString(raw[:])
	r.MAC = openMAC(key, r)
	return r, nil
}

func signReply(key []byte, r wire.CableReply) wire.CableReply {
	r.MAC = replyMAC(key, r)
	return r
}

// checkReply says the reply is authentic and answers the request with this nonce.
func checkReply(key []byte, r wire.CableReply, nonce string) error {
	if r.Nonce != nonce || !hmac.Equal([]byte(r.MAC), []byte(replyMAC(key, r))) {
		return errCableMAC
	}
	return nil
}

// validateCableOpen bounds the fields of an open request before anything else is done with it.
func validateCableOpen(r wire.CableOpen) error {
	for _, f := range []string{r.Namespace, r.Name, r.UID, r.Lab, r.LabUID, r.Serial.Device, r.Serial.Port, r.Console.Device, r.Console.Port} {
		if f == "" || len(f) > maxName || strings.ContainsRune(f, 0) {
			return errors.New("cable request: a field is empty, too long or holds NUL")
		}
	}
	if len(r.Nonce) != 32 || len(r.MAC) != 64 {
		return errors.New("cable request: bad nonce or MAC")
	}
	if _, err := hex.DecodeString(r.Nonce); err != nil {
		return errors.New("cable request: bad nonce")
	}
	if r.V != wire.RelayVersion {
		return fmt.Errorf("cable request: version %d", r.V)
	}
	return nil
}

// nonceCache remembers the nonces accepted within twice the window; a full cache refuses new requests rather than forget a nonce that is
// still inside the window.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	max  int
}

func newNonceCache(max int) *nonceCache { return &nonceCache{seen: map[string]time.Time{}, max: max} }

// add records the nonce; false when it was seen before or there is no room.
func (c *nonceCache) add(nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, at := range c.seen {
		if now.Sub(at) > 2*CableWindow {
			delete(c.seen, n)
		}
	}
	if _, ok := c.seen[nonce]; ok || len(c.seen) >= c.max {
		return false
	}
	c.seen[nonce] = now
	return true
}

// cableAuth checks open requests.
type cableAuth struct {
	key    *CableKey
	nonces *nonceCache
	now    func() time.Time
}

// check authenticates an open request: the MAC first (constant time), then the time, then the nonce (so an unauthentic request cannot
// fill the nonce cache). A failure never says which check failed.
func (a *cableAuth) check(r wire.CableOpen) error {
	key, err := a.key.Get()
	if err != nil {
		return err
	}
	if err := validateCableOpen(r); err != nil {
		return err
	}
	if !hmac.Equal([]byte(r.MAC), []byte(openMAC(key, r))) {
		return errCableMAC
	}
	now := a.now()
	if d := now.Sub(time.UnixMilli(r.Time)); d > CableWindow || d < -CableWindow {
		return errCableTime
	}
	if !a.nonces.add(r.Nonce, now) {
		return errCableReplay
	}
	return nil
}
