package wire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Authentication of an open request. The listener of the service is protected by a network policy, and in addition every open message
// carries an HMAC-SHA256 made by the proxy with a key only the proxy and the service hold: over every field of the request, a timestamp and
// a random nonce. The service checks it first, with a constant-time comparison, then the timestamp window and the nonce, before it does
// anything else with the request.

// MinKeyLen is the shortest accepted key.
const MinKeyLen = 32

// Canonical is the byte string that is signed: every field of the request but the signature, each length-prefixed, in a fixed order, so
// that no two different requests have the same string.
func Canonical(r OpenRequest) []byte {
	var b []byte
	str := func(s string) { b = binary.AppendUvarint(b, uint64(len(s))); b = append(b, s...) }
	num := func(n int64) { b = binary.AppendVarint(b, n) }
	str("cib-device-access-open-v1")
	str(r.Kind)
	str(r.Namespace)
	str(r.Lab)
	str(r.Device)
	num(int64(len(r.Command)))
	for _, c := range r.Command {
		str(c)
	}
	str(r.User)
	num(int64(r.Cols))
	num(int64(r.Rows))
	str(r.Path)
	num(r.Size)
	str(r.Client)
	str(r.Group)
	num(r.Time)
	str(r.Nonce)
	return b
}

func mac(key []byte, r OpenRequest) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(Canonical(r))
	return h.Sum(nil)
}

// Sign sets the timestamp, a fresh nonce and the signature of r.
func Sign(key []byte, r *OpenRequest, now time.Time) error {
	if len(key) < MinKeyLen {
		return errors.New("the key is too short")
	}
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return err
	}
	r.Time, r.Nonce, r.Sig = now.Unix(), hex.EncodeToString(n[:]), ""
	r.Sig = hex.EncodeToString(mac(key, *r))
	return nil
}

// VerifySignature reports whether r is signed with one of the keys. It does not look at the time or the nonce.
func VerifySignature(keys [][]byte, r OpenRequest) bool {
	got, err := hex.DecodeString(r.Sig)
	if err != nil || len(got) != sha256.Size || r.Nonce == "" {
		return false
	}
	ok := false
	for _, k := range keys {
		// every key is tried, so the time taken does not tell which one matched
		if hmac.Equal(got, mac(k, r)) {
			ok = true
		}
	}
	return ok
}

// KeyFiles holds the keys read from files, and reads them again from time to time: a Secret mounted as files changes in place when it is
// updated, so a rotation needs no restart. The first file is the key the proxy signs with; the service accepts all of them.
type KeyFiles struct {
	paths   []string
	every   time.Duration
	mu      sync.Mutex
	keys    [][]byte
	checked time.Time
}

// NewKeyFiles reads the files. The first (current) must exist and hold at least MinKeyLen bytes; the others are optional (a missing file
// is no key) but one that exists must be long enough.
func NewKeyFiles(every time.Duration, paths ...string) (*KeyFiles, error) {
	k := &KeyFiles{paths: paths, every: every}
	keys, err := readKeys(paths)
	if err != nil {
		return nil, err
	}
	k.keys, k.checked = keys, time.Now()
	return k, nil
}

func readKeys(paths []string) ([][]byte, error) {
	var keys [][]byte
	for i, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			if i > 0 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read the key file: %w", err)
		}
		key := []byte(strings.TrimRight(string(raw), "\r\n"))
		if len(key) < MinKeyLen {
			return nil, fmt.Errorf("the key in %s is shorter than %d bytes", p, MinKeyLen)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (k *KeyFiles) current() [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Since(k.checked) >= k.every {
		k.checked = time.Now()
		// a bad or half-written file keeps the keys already held
		if keys, err := readKeys(k.paths); err == nil {
			k.keys = keys
		}
	}
	return k.keys
}

// Signing is the key the proxy signs with.
func (k *KeyFiles) Signing() []byte { return k.current()[0] }

// Accepted are the keys the service accepts.
func (k *KeyFiles) Accepted() [][]byte { return k.current() }
