package egressproxy

import (
	"errors"
	"io"
)

// What the proxy needs from the first flight of a TLS connection: the server name the client asks for, and whether it hides it (Encrypted
// ClientHello). The proxy never terminates TLS and never reads past the ClientHello.

// extension numbers
const (
	extServerName = 0x0000
	// extECH is encrypted_client_hello (RFC 9849); extECHOuter the extension of the drafts (outer_extension) and extECHDraft the draft
	// numbers a client may still send. A ClientHello with any of them hides the real server name.
	extECH      = 0xfe0d
	extECHDraft = 0xfe08
	extECHOld   = 0xff03
)

var (
	errNotTLS     = errors.New("not a TLS ClientHello")
	errShortHello = errors.New("the ClientHello is cut short")
)

// ClientHello is what was read from the first flight.
type ClientHello struct {
	// ServerName is the host_name of the server_name extension, lower case, "" when there is none.
	ServerName string
	// ECH is true when the client sent an encrypted_client_hello extension.
	ECH bool
	// Raw are the bytes read from the connection, to be sent on to the server unchanged.
	Raw []byte
}

const maxHelloBytes = 64 << 10

// ReadClientHello reads TLS records from r until one handshake message (the ClientHello) is complete, and parses it. The bytes it consumed
// are in Raw. A flight that is not a ClientHello, or is larger than 64 kB, is an error.
func ReadClientHello(r io.Reader) (*ClientHello, error) {
	var raw, msg []byte
	need := -1 // the length of the handshake message once its 4-byte header is known
	for {
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, err
		}
		raw = append(raw, hdr...)
		// content type 22 = handshake; version 0x03xx
		if hdr[0] != 22 || hdr[1] != 3 {
			return nil, errNotTLS
		}
		n := int(hdr[3])<<8 | int(hdr[4])
		if n == 0 || n > 1<<14+256 || len(raw)+n > maxHelloBytes {
			return nil, errNotTLS
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		raw = append(raw, body...)
		msg = append(msg, body...)
		if need < 0 && len(msg) >= 4 {
			if msg[0] != 1 { // client_hello
				return nil, errNotTLS
			}
			need = int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
			if need > maxHelloBytes {
				return nil, errNotTLS
			}
		}
		if need >= 0 && len(msg) >= 4+need {
			h, err := parseHello(msg[4 : 4+need])
			if err != nil {
				return nil, err
			}
			h.Raw = raw
			return h, nil
		}
	}
}

type reader struct{ b []byte }

func (r *reader) u8() (int, bool) {
	if len(r.b) < 1 {
		return 0, false
	}
	v := int(r.b[0])
	r.b = r.b[1:]
	return v, true
}

func (r *reader) u16() (int, bool) {
	if len(r.b) < 2 {
		return 0, false
	}
	v := int(r.b[0])<<8 | int(r.b[1])
	r.b = r.b[2:]
	return v, true
}

func (r *reader) take(n int) ([]byte, bool) {
	if n < 0 || len(r.b) < n {
		return nil, false
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v, true
}

func parseHello(b []byte) (*ClientHello, error) {
	r := &reader{b: b}
	if _, ok := r.take(2 + 32); !ok { // version, random
		return nil, errShortHello
	}
	n, ok := r.u8() // session id
	if !ok {
		return nil, errShortHello
	}
	if _, ok := r.take(n); !ok {
		return nil, errShortHello
	}
	n, ok = r.u16() // cipher suites
	if !ok {
		return nil, errShortHello
	}
	if _, ok := r.take(n); !ok {
		return nil, errShortHello
	}
	n, ok = r.u8() // compression
	if !ok {
		return nil, errShortHello
	}
	if _, ok := r.take(n); !ok {
		return nil, errShortHello
	}
	h := &ClientHello{}
	if len(r.b) == 0 { // no extensions at all: no server name
		return h, nil
	}
	n, ok = r.u16()
	if !ok {
		return nil, errShortHello
	}
	exts, ok := r.take(n)
	if !ok {
		return nil, errShortHello
	}
	er := &reader{b: exts}
	for len(er.b) > 0 {
		typ, ok1 := er.u16()
		l, ok2 := er.u16()
		if !ok1 || !ok2 {
			return nil, errShortHello
		}
		data, ok := er.take(l)
		if !ok {
			return nil, errShortHello
		}
		switch typ {
		case extServerName:
			h.ServerName = parseServerName(data)
		case extECH, extECHDraft, extECHOld:
			h.ECH = true
		}
	}
	return h, nil
}

// parseServerName reads the first host_name (type 0) of a server_name extension.
func parseServerName(b []byte) string {
	r := &reader{b: b}
	l, ok := r.u16()
	if !ok {
		return ""
	}
	list, ok := r.take(l)
	if !ok {
		return ""
	}
	lr := &reader{b: list}
	for len(lr.b) > 0 {
		typ, ok1 := lr.u8()
		n, ok2 := lr.u16()
		if !ok1 || !ok2 {
			return ""
		}
		name, ok := lr.take(n)
		if !ok {
			return ""
		}
		if typ == 0 {
			return string(name)
		}
	}
	return ""
}
