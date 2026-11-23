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

// The authentication of the cable stream between the device access services of two nodes. Only the device access pods hold the master key
// (a Secret of their namespace that nothing else mounts; it is not the access key of a tenant and not the grant of a console). Each node
// signs with its node key, HMAC(master, node name), and names its node in the open request; the acceptor derives that node key, checks the
// MAC in constant time, the time within CableWindow of its own clock, that the nonce was not seen, and that the dialer's node is the node
// of the console end of the line its own node-agent registered. Its reply is signed over the request's nonce and its own nonce, and the
// session keys of the stream (cable_session.go) are derived from the node key and both nonces: a replayed open, even one whose original
// never arrived, yields a stream the replayer can neither read nor write. Authentication is not authorization: an accepted request is
// served only for a line the acceptor's node-agent gave it with the serial role and the same cable, UID, lab, lab UID and ends.

// CableWindow is how far the time of an open request may be from the acceptor's clock.
const CableWindow = 30 * time.Second

// minCableKey is the shortest key accepted (bytes of the file after trimming); maxNonces bounds the nonces remembered and
// maxNoncesPerSource the nonces of one source address, so one source can never fill the cache for the others.
const (
	minCableKey        = 32
	maxNonces          = 20000
	maxNoncesPerSource = 2000
)

var (
	errCableKey    = errors.New("no usable cable key")
	errCableMAC    = errors.New("the cable request is not authentic")
	errCableTime   = errors.New("the cable request is outside the time window")
	errCableReplay = errors.New("the nonce of the cable request was seen before")
)

// CableKey reads the master key from a file and, during a rotation, the previous key from a second file, again whenever a file changes
// (a rotated Secret reaches the pod without a restart). Requests are signed with the current key and accepted with either.
type CableKey struct {
	paths []string

	mu    sync.Mutex
	files []keyFile
	fixed [][]byte
}

type keyFile struct {
	key   []byte
	mtime time.Time
	size  int64
}

// NewCableKey returns the key of the file at path; previous, when not empty, is the file of the key being rotated out (it may be absent).
func NewCableKey(path, previous string) *CableKey {
	k := &CableKey{paths: []string{path}}
	if previous != "" {
		k.paths = append(k.paths, previous)
	}
	k.files = make([]keyFile, len(k.paths))
	return k
}

// StaticCableKey is a key held in memory (tests); more keys are previous keys.
func StaticCableKey(keys ...[]byte) *CableKey {
	k := &CableKey{}
	for _, key := range keys {
		k.fixed = append(k.fixed, append([]byte(nil), key...))
	}
	return k
}

// Keys returns the usable keys, the current one first; an error when the current one is missing or too short.
func (k *CableKey) Keys() ([][]byte, error) {
	if k == nil {
		return nil, errCableKey
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.paths == nil {
		var out [][]byte
		for _, key := range k.fixed {
			if len(key) >= minCableKey {
				out = append(out, key)
			} else if len(out) == 0 {
				return nil, errCableKey
			}
		}
		if len(out) == 0 {
			return nil, errCableKey
		}
		return out, nil
	}
	var out [][]byte
	for i, p := range k.paths {
		key := k.read(i, p)
		if key == nil {
			if i == 0 {
				return nil, errCableKey
			}
			continue
		}
		out = append(out, key)
	}
	return out, nil
}

func (k *CableKey) read(i int, path string) []byte {
	st, err := os.Stat(path)
	if err != nil {
		k.files[i] = keyFile{}
		return nil
	}
	f := &k.files[i]
	if f.key == nil || !st.ModTime().Equal(f.mtime) || st.Size() != f.size {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		key := []byte(strings.TrimSpace(string(raw)))
		if len(key) < minCableKey {
			*f = keyFile{}
			return nil
		}
		*f = keyFile{key: key, mtime: st.ModTime(), size: st.Size()}
	}
	return f.key
}

// macOf joins the fields of a MAC: a NUL never appears in a field (validateCableOpen refuses it), so the encoding is unambiguous.
func macOf(key []byte, parts ...string) string {
	return hex.EncodeToString(macBytes(key, parts...))
}

func macBytes(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		_, _ = m.Write([]byte(p))
		_, _ = m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// nodeKey is the key a node signs with: HMAC(master, node name).
func nodeKey(master []byte, node string) []byte { return macBytes(master, "cib-cable-node", node) }

func openMAC(key []byte, r wire.CableOpen) string {
	return macOf(key, "cib-cable-open", strconv.Itoa(r.V), r.Node, r.Namespace, r.Name, r.UID, r.Lab, r.LabUID,
		r.Serial.Device, r.Serial.Port, r.Console.Device, r.Console.Port, strconv.FormatInt(r.Time, 10), r.Nonce)
}

func replyMAC(key []byte, r wire.CableReply) string {
	return macOf(key, "cib-cable-reply", r.Nonce, r.Accept, strconv.FormatBool(r.OK), r.Error)
}

// newNonce is 16 random bytes in hex.
func newNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// validNonce says s is 32 lowercase hex characters (the shape newNonce makes).
func validNonce(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// signOpen fills the version, the dialer's node, the time, a fresh nonce and the MAC of an open request; it returns the node key it
// signed with (the session is derived from it).
func signOpen(master []byte, node string, r wire.CableOpen, now time.Time) (wire.CableOpen, []byte, error) {
	nonce, err := newNonce()
	if err != nil {
		return r, nil, err
	}
	key := nodeKey(master, node)
	r.V, r.Node, r.Time, r.Nonce = wire.RelayVersion, node, now.UnixMilli(), nonce
	r.MAC = openMAC(key, r)
	return r, key, nil
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
	if r.OK && !validNonce(r.Accept) {
		return errCableMAC
	}
	return nil
}

// validateCableOpen bounds the fields of an open request before anything else is done with it.
func validateCableOpen(r wire.CableOpen) error {
	for _, f := range []string{r.Node, r.Namespace, r.Name, r.UID, r.Lab, r.LabUID, r.Serial.Device, r.Serial.Port, r.Console.Device, r.Console.Port} {
		if f == "" || len(f) > maxName || strings.ContainsRune(f, 0) {
			return errors.New("cable request: a field is empty, too long or holds NUL")
		}
	}
	if !validNonce(r.Nonce) || len(r.MAC) != 64 {
		return errors.New("cable request: bad nonce or MAC")
	}
	if r.V != wire.RelayVersion {
		return fmt.Errorf("cable request: version %d", r.V)
	}
	return nil
}

// nonceCache remembers the nonces accepted within twice the window. One source address holds at most perSource of them (its oldest goes
// first); when the whole cache is full the oldest nonce goes, so a flood never locks the lines of everybody else. A nonce forgotten that
// way could be replayed inside its window, and the replay would still get nothing: the session keys bind the acceptor's fresh nonce.
type nonceCache struct {
	mu        sync.Mutex
	seen      map[string]nonceEntry
	bySource  map[string]int
	max       int
	perSource int
}

type nonceEntry struct {
	at     time.Time
	source string
}

func newNonceCache(max, perSource int) *nonceCache {
	return &nonceCache{seen: map[string]nonceEntry{}, bySource: map[string]int{}, max: max, perSource: perSource}
}

func (c *nonceCache) drop(n string) {
	e := c.seen[n]
	delete(c.seen, n)
	if c.bySource[e.source]--; c.bySource[e.source] <= 0 {
		delete(c.bySource, e.source)
	}
}

// oldest is the oldest nonce, of one source when source is not empty.
func (c *nonceCache) oldest(source string) string {
	var name string
	var at time.Time
	for n, e := range c.seen {
		if source != "" && e.source != source {
			continue
		}
		if name == "" || e.at.Before(at) {
			name, at = n, e.at
		}
	}
	return name
}

// add records the nonce of a source; false when it was seen before.
func (c *nonceCache) add(nonce, source string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, e := range c.seen {
		if now.Sub(e.at) > 2*CableWindow {
			c.drop(n)
		}
	}
	if _, ok := c.seen[nonce]; ok {
		return false
	}
	if c.bySource[source] >= c.perSource {
		c.drop(c.oldest(source))
	}
	if len(c.seen) >= c.max {
		c.drop(c.oldest(""))
	}
	c.seen[nonce] = nonceEntry{at: now, source: source}
	c.bySource[source]++
	return true
}

// cableAuth checks open requests.
type cableAuth struct {
	key    *CableKey
	nonces *nonceCache
	now    func() time.Time
}

// check authenticates an open request from a source address: the shape, the MAC with the node key of the named node under any current
// key (constant time), then the time, then the nonce (so an unauthentic request cannot fill the nonce cache). It returns the node key that
// verified it. A failure never says which check failed.
func (a *cableAuth) check(r wire.CableOpen, source string) ([]byte, error) {
	keys, err := a.key.Keys()
	if err != nil {
		return nil, err
	}
	if err := validateCableOpen(r); err != nil {
		return nil, err
	}
	var key []byte
	for _, master := range keys {
		nk := nodeKey(master, r.Node)
		if hmac.Equal([]byte(r.MAC), []byte(openMAC(nk, r))) {
			key = nk
			break
		}
	}
	if key == nil {
		return nil, errCableMAC
	}
	now := a.now()
	if d := now.Sub(time.UnixMilli(r.Time)); d > CableWindow || d < -CableWindow {
		return nil, errCableTime
	}
	if !a.nonces.add(r.Nonce, source, now) {
		return nil, errCableReplay
	}
	return key, nil
}
