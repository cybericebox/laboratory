package deviceaccess

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The authentication of the cable stream between the device access services of two nodes.
//
// Every device access process makes its own Ed25519 identity at start and keeps the private key in memory only (it never touches a disk
// and dies with the process). It gives the public key to the node-agent of its node over cable.sock; the node-agent publishes it on its
// own Node (annotation laboratory.cybericebox.com/console-relay-key; the admission policy of the node-agent lets a node-agent set it on its
// own node only). The node-agent of the serial end's node gives its service, with each line, the key published on the console end's node;
// the owner's node-agent gives its service the key of the serial end's node. So a node holds no secret of another node: a compromised node
// can sign only as itself, and is served only for the lines whose console end runs on it.
//
// The handshake:
//
//   - The dialer makes an ephemeral X25519 key and sends CableOpen: its node, the cable, its UID, the lab and lab UID, both ends, its clock,
//     a 16-byte nonce and the ephemeral public key, signed with its identity.
//   - The acceptor finds the line its own node-agent gave it with the serial role, the same cable, lab and ends and that dialer node; it
//     checks the signature with that node's published key, the time (within CableWindow of its clock, and not before the process started),
//     and that the nonce is new for that node (no older than an evicted one of the same node). Only then does it sign anything: the reply
//     carries its nonce and its own ephemeral key, signed with its identity over the digest of the open and the reply.
//   - Both sides compute the X25519 secret of the two ephemeral keys; the session keys (cable_session.go) are HMACs of it over the digest of
//     both signed messages. A replayed open gets a reply for an ephemeral key whose private half the replayer does not have: it can neither
//     read nor write the stream, and the reply's signature covers the acceptor's fresh key, so it is of no use elsewhere either.
//
// Authentication is not authorization: an accepted open is served only for a line the acceptor's node-agent gave it.

// CableWindow is how far the time of an open request may be from the acceptor's clock.
const CableWindow = 30 * time.Second

// maxNonces bounds the nonces remembered and maxNoncesPerSource the nonces of one dialer node, so one node can never fill the cache for the
// others.
const (
	maxNonces          = 20000
	maxNoncesPerSource = 2000
)

var (
	errCableSig    = errors.New("the cable message is not signed by the expected node")
	errCableTime   = errors.New("the cable request is outside the time window")
	errCableReplay = errors.New("the nonce of the cable request was seen before, or is older than one forgotten")
)

var b64 = base64.RawURLEncoding

// Identity is the Ed25519 key pair of one device access process.
type Identity struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewIdentity makes a fresh identity.
func NewIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{priv: priv, pub: pub}, nil
}

// PublicKey is the public key as the node-agent publishes it (base64url without padding, 43 characters).
func (id *Identity) PublicKey() string { return b64.EncodeToString(id.pub) }

func (id *Identity) sign(msg []byte) string { return b64.EncodeToString(ed25519.Sign(id.priv, msg)) }

// ParsePublicKey reads a published key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := b64.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not an Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

func verifySig(pubKey, sig string, msg []byte) error {
	pub, err := ParsePublicKey(pubKey)
	if err != nil {
		return errCableSig
	}
	raw, err := b64.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize || !ed25519.Verify(pub, msg, raw) {
		return errCableSig
	}
	return nil
}

// transcript joins the fields of a signed message: a NUL never appears in a field (validateCableOpen refuses it), so it is unambiguous.
func transcript(parts ...string) []byte {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return []byte(b.String())
}

func openTranscript(r wire.CableOpen) []byte {
	return transcript("cib-cable-open/3", strconv.Itoa(r.V), r.Node, r.Namespace, r.Name, r.UID, r.Lab, r.LabUID,
		r.Serial.Device, r.Serial.Port, r.Console.Device, r.Console.Port, strconv.FormatInt(r.Time, 10), r.Nonce, r.Eph)
}

func replyTranscript(open []byte, r wire.CableReply) []byte {
	sum := sha256.Sum256(open)
	return transcript("cib-cable-reply/3", hex.EncodeToString(sum[:]), r.Nonce, r.Accept, strconv.FormatBool(r.OK), r.Error, r.Eph)
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

// ephemeral is one side's X25519 key of a handshake.
type ephemeral struct{ key *ecdh.PrivateKey }

func newEphemeral() (*ephemeral, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &ephemeral{key: k}, nil
}

func (e *ephemeral) public() string { return b64.EncodeToString(e.key.PublicKey().Bytes()) }

// shared is the X25519 secret with the peer's ephemeral key (an all-zero result, a low-order peer key, is an error).
func (e *ephemeral) shared(peer string) ([]byte, error) {
	raw, err := b64.DecodeString(peer)
	if err != nil {
		return nil, errCableSig
	}
	pk, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, errCableSig
	}
	return e.key.ECDH(pk)
}

// signOpen fills the version, the dialer's node, the time, a fresh nonce and ephemeral key, and the signature of an open request.
func signOpen(id *Identity, node string, r wire.CableOpen, now time.Time) (wire.CableOpen, *ephemeral, error) {
	nonce, err := newNonce()
	if err != nil {
		return r, nil, err
	}
	eph, err := newEphemeral()
	if err != nil {
		return r, nil, err
	}
	r.V, r.Node, r.Time, r.Nonce, r.Eph = wire.RelayVersion, node, now.UnixMilli(), nonce, eph.public()
	r.Sig = id.sign(openTranscript(r))
	return r, eph, nil
}

// signReply signs a reply to the open.
func signReply(id *Identity, open []byte, r wire.CableReply) wire.CableReply {
	r.Sig = id.sign(replyTranscript(open, r))
	return r
}

// checkReply says the reply is signed by the peer's key and answers the request with this nonce.
func checkReply(peerKey string, open []byte, r wire.CableReply, nonce string) error {
	if r.Nonce != nonce {
		return errCableSig
	}
	if err := verifySig(peerKey, r.Sig, replyTranscript(open, r)); err != nil {
		return err
	}
	if r.OK && (!validNonce(r.Accept) || r.Eph == "") {
		return errCableSig
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
	if !validNonce(r.Nonce) || len(r.Eph) != 43 || len(r.Sig) != 86 {
		return errors.New("cable request: bad nonce, key or signature")
	}
	if r.V != wire.RelayVersion {
		return fmt.Errorf("cable request: version %d", r.V)
	}
	return nil
}

// nonceCache remembers the nonces of the opens accepted within twice the window, per dialer node (the node the signature proved). One node
// holds at most perSource of them and loses its oldest first; a full cache loses the oldest of all; it never refuses for room, so a flood
// never locks the lines of the others. A forgotten nonce must not be replayable inside its window: the cache keeps, per node, the time of
// the newest open it forgot (the floor) and refuses every open of that node that is not newer.
type nonceCache struct {
	mu        sync.Mutex
	seen      map[string]nonceEntry
	bySource  map[string]int
	floor     map[string]int64 // node -> open time (Unix ms) of the newest forgotten nonce
	max       int
	perSource int
}

type nonceEntry struct {
	at     time.Time
	source string
	open   int64
}

func newNonceCache(max, perSource int) *nonceCache {
	return &nonceCache{seen: map[string]nonceEntry{}, bySource: map[string]int{}, floor: map[string]int64{}, max: max, perSource: perSource}
}

// forget drops a nonce that may still be inside its window and raises the floor of its node.
func (c *nonceCache) forget(n string) {
	e := c.seen[n]
	c.floor[e.source] = max(c.floor[e.source], e.open)
	c.drop(n)
}

func (c *nonceCache) drop(n string) {
	e := c.seen[n]
	delete(c.seen, n)
	if c.bySource[e.source]--; c.bySource[e.source] <= 0 {
		delete(c.bySource, e.source)
	}
}

// oldest is the nonce with the oldest open time, of one node when source is not empty.
func (c *nonceCache) oldest(source string) string {
	var name string
	var open int64
	for n, e := range c.seen {
		if source != "" && e.source != source {
			continue
		}
		if name == "" || e.open < open {
			name, open = n, e.open
		}
	}
	return name
}

// add records the nonce of an open of a node at open time openMs; false when it was seen before or is not newer than a forgotten one.
func (c *nonceCache) add(nonce, source string, openMs int64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, e := range c.seen {
		// past twice the window an open is refused by its time anyway: no floor needed
		if now.Sub(e.at) > 2*CableWindow {
			c.drop(n)
		}
	}
	for s, f := range c.floor {
		if now.Sub(time.UnixMilli(f)) > 2*CableWindow {
			delete(c.floor, s)
		}
	}
	if _, ok := c.seen[nonce]; ok || openMs <= c.floor[source] {
		return false
	}
	if c.bySource[source] >= c.perSource {
		c.forget(c.oldest(source))
	}
	if len(c.seen) >= c.max {
		c.forget(c.oldest(""))
	}
	c.seen[nonce] = nonceEntry{at: now, source: source, open: openMs}
	c.bySource[source]++
	return true
}

// cableAuth checks open requests.
type cableAuth struct {
	id     *Identity
	nonces *nonceCache
	now    func() time.Time
	// started is when this process started: an open signed before that is refused (it could be a replay of one the previous process saw).
	started time.Time
}

// check authenticates an open request for a line whose console end is the node with the published key nodeKey: the signature, then the
// time (inside the window and not before this process started), then the nonce (so an unauthentic request cannot fill the nonce cache).
// A failure never says which check failed.
func (a *cableAuth) check(r wire.CableOpen, nodeKey string) error {
	if err := validateCableOpen(r); err != nil {
		return err
	}
	if err := verifySig(nodeKey, r.Sig, openTranscript(r)); err != nil {
		return err
	}
	now := a.now()
	t := time.UnixMilli(r.Time)
	// the time is in milliseconds: an open of the same millisecond as the start is not "before" it
	if d := now.Sub(t); d > CableWindow || d < -CableWindow || r.Time < a.started.UnixMilli() {
		return errCableTime
	}
	if !a.nonces.add(r.Nonce, r.Node, r.Time, now) {
		return errCableReplay
	}
	return nil
}

// sessionSecret is the base of the session keys: the X25519 secret, bound to both signed messages.
func sessionSecret(shared, open, reply []byte) []byte {
	sum := sha256.Sum256(append(append([]byte(nil), open...), reply...))
	m := hmac.New(sha256.New, shared)
	_, _ = m.Write([]byte("cib-cable-session/3"))
	_, _ = m.Write(sum[:])
	return m.Sum(nil)
}
