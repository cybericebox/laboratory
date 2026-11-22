package portkeeper

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
)

// AttachOK is the one byte the keeper sends a client it accepted, before any byte of the line. A client that reads EOF instead was
// refused: the line has a client already (one line, one cable). After the byte the stream is the raw bytes of the line both ways.
const AttachOK = 0x06

// Hub is one port: it reads the line end (the PTY master, or a tcp console) all the time and hands the bytes to the one attached client,
// or drops them while none is attached, so the program on the line never blocks (an unplugged real line loses output too). What the
// client sends goes to the line end; with no line end (a tcp console that is not connected) it is dropped.
type Hub struct {
	mu     sync.Mutex
	client net.Conn
	line   io.Writer
}

// NewHub is a port with no client and no line end.
func NewHub() *Hub { return &Hub{} }

// SetLine sets the end the client's bytes go to (nil: none, they are dropped).
func (h *Hub) SetLine(w io.Writer) {
	h.mu.Lock()
	h.line = w
	h.mu.Unlock()
}

// Attached says a client holds the line.
func (h *Hub) Attached() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.client != nil
}

// WriteLine writes the client's bytes to the line end, or drops them when there is none.
func (h *Hub) WriteLine(p []byte) (int, error) {
	h.mu.Lock()
	w := h.line
	h.mu.Unlock()
	if w == nil {
		return len(p), nil
	}
	return w.Write(p)
}

// deliver hands output of the line to the client, or drops it. A client that cannot take it is detached.
func (h *Hub) deliver(p []byte) {
	h.mu.Lock()
	c := h.client
	h.mu.Unlock()
	if c == nil {
		return
	}
	if _, err := c.Write(p); err != nil {
		h.detach(c)
	}
}

// Pump reads the line end until it fails and delivers what it reads. It returns the read error (EOF or EIO when the end is gone).
func (h *Hub) Pump(r io.Reader) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.deliver(buf[:n])
		}
		if err != nil {
			return err
		}
	}
}

func (h *Hub) attach(c net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		return false
	}
	h.client = c
	return true
}

func (h *Hub) detach(c net.Conn) {
	h.mu.Lock()
	if h.client == c {
		h.client = nil
	}
	h.mu.Unlock()
	_ = c.Close()
}

// Serve accepts clients on ln until it is closed: the first one gets the line, any other is closed at once.
func (h *Hub) Serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !h.attach(c) {
			_ = c.Close()
			continue
		}
		// The handshake goes out under the client's slot, before any output of the line can reach it.
		h.mu.Lock()
		_, werr := c.Write([]byte{AttachOK})
		h.mu.Unlock()
		if werr != nil {
			h.detach(c)
			continue
		}
		go h.readClient(c)
	}
}

func (h *Hub) readClient(c net.Conn) {
	defer h.detach(c)
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := h.WriteLine(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Listen makes the unix socket of a port, readable and writable by its owner (root) only. A socket left by a keeper that died is removed.
func Listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
