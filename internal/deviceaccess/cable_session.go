package deviceaccess

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
)

// The sealed cable stream. After the handshake every binary message of the stream is one wire frame (Data or Close) sealed with
// AES-256-GCM. Each direction has its own key, HMAC(session secret, direction), where the session secret binds the X25519 secret of the two
// ephemeral keys to both signed handshake messages (cable_auth.go), and a sequence number that starts at 0 and grows by one per message;
// the 12-byte GCM nonce is 4 zero bytes and the sequence number, and the sequence number travels in front of the sealed frame and is
// authenticated with it. So nobody on the path can read a byte of the line, change, inject, drop, reorder or replay a frame, or splice
// frames of another session or of the other direction: the receiver accepts only the next sequence number under its own key and ends the
// line on anything else. The keys exist only in the two device access processes and only for the life of the session.

// sealOverhead is what sealing adds to a frame: the sequence number and the GCM tag.
const sealOverhead = 8 + 16

var errSealed = errors.New("a frame of the cable stream is not authentic or out of order")

type sealedConn struct {
	conn       Conn
	send, recv cipher.AEAD

	sendMu  sync.Mutex
	sendSeq uint64
	recvSeq uint64
}

func sessionAEAD(secret []byte, direction string) (cipher.AEAD, error) {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write([]byte(direction))
	key := m.Sum(nil) // 32 bytes: AES-256
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// newSealedConn is the sealed side of a stream over a session secret; dialer says which end this is.
func newSealedConn(conn Conn, secret []byte, dialer bool) (*sealedConn, error) {
	d2a, err := sessionAEAD(secret, "dialer-to-acceptor")
	if err != nil {
		return nil, err
	}
	a2d, err := sessionAEAD(secret, "acceptor-to-dialer")
	if err != nil {
		return nil, err
	}
	s := &sealedConn{conn: conn, send: a2d, recv: d2a}
	if dialer {
		s.send, s.recv = d2a, a2d
	}
	return s, nil
}

func gcmNonce(seq uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], seq)
	return n
}

// writeFrame seals one wire frame and sends it.
func (s *sealedConn) writeFrame(ctx context.Context, frame []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	seq := s.sendSeq
	s.sendSeq++
	msg := make([]byte, 8, 8+len(frame)+16)
	binary.BigEndian.PutUint64(msg, seq)
	msg = s.send.Seal(msg, gcmNonce(seq), frame, msg[:8])
	return s.conn.WriteBinary(ctx, msg)
}

// readFrame reads the next sealed message and returns its wire frame.
func (s *sealedConn) readFrame(ctx context.Context) ([]byte, error) {
	text, msg, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if text || len(msg) < sealOverhead {
		return nil, errSealed
	}
	seq := binary.BigEndian.Uint64(msg)
	if seq != s.recvSeq {
		return nil, errSealed
	}
	frame, err := s.recv.Open(nil, gcmNonce(seq), msg[8:], msg[:8])
	if err != nil {
		return nil, errSealed
	}
	s.recvSeq++
	return frame, nil
}
