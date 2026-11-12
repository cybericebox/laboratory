// Package wire is the framing of a console stream. The browser and the L7 proxy speak it over a WebSocket, and so do the proxy and
// the device access service: the proxy checks every frame it relays, so neither side ever hands a raw, unchecked message to the
// other. The package has no dependencies, so the proxy can import it without pulling in the container runtime.
//
// A connection starts with one text message (JSON): the open request towards the service, the ready message towards the browser.
// Everything after is a binary message: one type byte followed by the payload of that type.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frame types.
const (
	// TypeData carries terminal bytes in either direction (stdin to the device, output from it). Opaque to the platform.
	TypeData byte = 1
	// TypeResize carries the new size of the terminal: columns and rows, two big-endian uint16. Browser to device only.
	TypeResize byte = 2
	// TypeClose ends the stream. The payload is a short reason (one of the Reason constants), for the log only.
	TypeClose byte = 3
	// TypeAck tells the sender how many output bytes the browser has finished drawing (a big-endian uint32). It is the flow control of
	// the terminal and is spoken between the browser and the proxy only; the service never sees it.
	TypeAck byte = 4
)

// Limits of a frame. A frame is never larger than this, so a peer cannot make the other side allocate more.
const (
	// MaxData is the largest data payload.
	MaxData = 32 << 10
	// MaxFrame is the largest whole message (type byte included) that either side accepts.
	MaxFrame = MaxData + 16
	// MaxOpen is the largest open or ready message.
	MaxOpen = 8 << 10
	// MinSize and MaxSize bound the terminal size.
	MinSize, MaxSize = 1, 500
)

// Kinds of a stream the service serves.
const (
	KindConsole = "console"
	KindPut     = "put"
	KindGet     = "get"
)

// Close reasons. They are for the log and the metric label, never for the participant's eyes beyond a short word.
const (
	ReasonExit     = "exit"
	ReasonClient   = "client"
	ReasonIdle     = "idle"
	ReasonLifetime = "lifetime"
	ReasonAccess   = "access-lost"
	ReasonLimit    = "limit"
	ReasonError    = "error"
	ReasonShutdown = "shutdown"
	ReasonStalled  = "stalled"
	ReasonRefused  = "refused"
	ReasonTooBig   = "too-big"
)

// OpenRequest is the first message the proxy sends to the service. The caller names a device and nothing else about the
// container: the service finds the container itself and accepts no container id, pod name or program path.
type OpenRequest struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Lab       string `json:"lab"`
	Device    string `json:"device"`
	// Command and User are the terminal settings of the device, as the lab author set them (empty: /bin/sh, and the user of the
	// image). They are checked again in the service. Only a console uses them; a file transfer runs as the same user.
	Command []string `json:"command,omitempty"`
	User    string   `json:"user,omitempty"`
	// Cols and Rows are the initial size of the terminal (console only).
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	// Path is the file of a put or a get. Size is the length of the upload.
	Path string `json:"path,omitempty"`
	Size int64  `json:"size,omitempty"`
	// Client and Group are the identifiers the proxy knows the participant by (a pseudonym, never an address). The service logs them and
	// counts consoles per client; it does not interpret them.
	Client string `json:"client,omitempty"`
	Group  string `json:"group,omitempty"`
}

// OpenReply answers an open request.
type OpenReply struct {
	OK bool `json:"ok"`
	// Error is a short code when OK is false: "not-found", "disabled", "limit", "bad-request", "unavailable".
	Error string `json:"error,omitempty"`
	// Size is the length of the file a get is about to send.
	Size int64 `json:"size,omitempty"`
}

// ErrFrame is returned for a message that is not a valid frame.
var ErrFrame = errors.New("invalid frame")

// Data builds a data frame.
func Data(p []byte) []byte { return append([]byte{TypeData}, p...) }

// Resize builds a resize frame.
func Resize(cols, rows uint16) []byte {
	b := []byte{TypeResize, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[1:], cols)
	binary.BigEndian.PutUint16(b[3:], rows)
	return b
}

// Close builds a close frame.
func Close(reason string) []byte { return append([]byte{TypeClose}, reason...) }

// Ack builds an acknowledgement frame.
func Ack(n uint32) []byte {
	b := []byte{TypeAck, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], n)
	return b
}

// Parse splits a message into its type and payload and checks it against the rules of the type. The payload aliases msg.
func Parse(msg []byte) (typ byte, payload []byte, err error) {
	if len(msg) == 0 || len(msg) > MaxFrame {
		return 0, nil, ErrFrame
	}
	typ, payload = msg[0], msg[1:]
	switch typ {
	case TypeData:
		if len(payload) > MaxData {
			return 0, nil, ErrFrame
		}
	case TypeResize:
		if len(payload) != 4 {
			return 0, nil, ErrFrame
		}
		if _, _, err := Size(payload); err != nil {
			return 0, nil, err
		}
	case TypeClose:
		if len(payload) > 64 {
			return 0, nil, ErrFrame
		}
	case TypeAck:
		if len(payload) != 4 {
			return 0, nil, ErrFrame
		}
	default:
		return 0, nil, fmt.Errorf("%w: type %d", ErrFrame, typ)
	}
	return typ, payload, nil
}

// Size reads a resize payload and checks the bounds.
func Size(payload []byte) (cols, rows uint16, err error) {
	if len(payload) != 4 {
		return 0, 0, ErrFrame
	}
	cols, rows = binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:])
	if cols < MinSize || cols > MaxSize || rows < MinSize || rows > MaxSize {
		return 0, 0, fmt.Errorf("%w: size %dx%d", ErrFrame, cols, rows)
	}
	return cols, rows, nil
}

// AckValue reads an acknowledgement payload.
func AckValue(payload []byte) uint32 { return binary.BigEndian.Uint32(payload) }
