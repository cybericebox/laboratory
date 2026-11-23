package portkeeper

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// AttachOK is the one byte the keeper sends a client it accepted, before any byte of the line. A client that reads EOF instead was
// refused: the line has a client already (one line, one cable). After the byte the stream is the raw bytes of the line both ways.
const AttachOK = 0x06

const (
	// clientQueue bounds the output waiting for a slow client (chunks of at most 4 KiB): when it is full the oldest chunk is dropped, as
	// an overrun serial line loses bytes. The program on the line never waits for a client.
	clientQueue = 64
	// clientWriteTimeout detaches a client that has taken nothing for this long.
	clientWriteTimeout = 5 * time.Second
	// acceptBackoffMax bounds the pause after repeated accept errors (EMFILE), so the loop never spins.
	acceptBackoffMax = time.Second
)

// Hub is one port: it reads the line end (the PTY master, or a tcp console) all the time and hands the bytes to the one attached client,
// or drops them while none is attached, so the program on the line never blocks (an unplugged real line loses output too). What the
// client sends goes to the line end; with no line end (a tcp console that is not connected) it is dropped.
type Hub struct {
	mu     sync.Mutex
	client *client
	line   io.Writer
	// attached, when made by AttachEvents, gets a signal each time a client attaches (the kernel log writes its ring buffer then).
	attached chan struct{}
	// gen counts the attaches: each client has its own number (Client, InjectFor).
	gen uint64
}

// client is the attached connection and its writer, which sends the handshake before it takes anything from the queue.
type client struct {
	conn net.Conn
	gen  uint64
	out  chan []byte
	// done is closed by detach: the writer stops, so a detached client leaves no goroutine and no queue behind.
	done chan struct{}
	once sync.Once
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

// AttachEvents is a channel that is signalled when a client attaches (signals do not pile up: one waiting signal at most).
func (h *Hub) AttachEvents() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.attached == nil {
		h.attached = make(chan struct{}, 1)
	}
	return h.attached
}

// Client is the number of the attached client (it changes with every attach), and whether one is attached.
func (h *Hub) Client() (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client == nil {
		return 0, false
	}
	return h.client.gen, true
}

// InjectFor queues bytes for the client with that number as if the line had sent them (in chunks of the queue's size); they are dropped
// when another client, or none, holds the line by then, so what was read for one client never reaches the next.
func (h *Hub) InjectFor(gen uint64, p []byte) {
	const chunk = 4096
	for len(p) > 0 {
		n := min(len(p), chunk)
		h.deliverTo(gen, p[:n])
		p = p[n:]
	}
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

// deliver queues output of the line for the client, or drops it; it never blocks. A full queue loses its oldest chunk.
func (h *Hub) deliver(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queue(h.client, p)
}

// deliverTo is deliver for the client with that number only.
func (h *Hub) deliverTo(gen uint64, p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil && h.client.gen == gen {
		h.queue(h.client, p)
	}
}

// queue puts a copy of p in the queue of c (h.mu held).
func (h *Hub) queue(c *client, p []byte) {
	if c == nil {
		return
	}
	b := append([]byte(nil), p...)
	for {
		select {
		case c.out <- b:
			return
		default:
		}
		select {
		case <-c.out: // drop the oldest
		default:
		}
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

// attach makes conn the client. Output of the line is queued from now on; the writer sends the handshake first, so it always comes first
// (it is not in the queue, so a full queue cannot drop it).
func (h *Hub) attach(conn net.Conn) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		return nil
	}
	h.gen++
	c := &client{conn: conn, gen: h.gen, out: make(chan []byte, clientQueue), done: make(chan struct{})}
	h.client = c
	if h.attached != nil {
		select {
		case h.attached <- struct{}{}:
		default:
		}
	}
	return c
}

func (h *Hub) detach(c *client) {
	h.mu.Lock()
	if h.client == c {
		h.client = nil
	}
	h.mu.Unlock()
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// write sends the queue to the client; a write that does not finish within the timeout detaches it.
func (h *Hub) write(c *client) {
	defer h.detach(c)
	_ = c.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
	if _, err := c.conn.Write([]byte{AttachOK}); err != nil {
		return
	}
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			_ = c.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
			if _, err := c.conn.Write(b); err != nil {
				return
			}
		}
	}
}

// Serve accepts clients on ln until it is closed: the first one gets the line, any other is closed at once.
func (h *Hub) Serve(ln net.Listener) {
	pause := time.Duration(0)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// out of descriptors or another passing error: wait before the next accept instead of spinning
			pause = min(max(2*pause, 5*time.Millisecond), acceptBackoffMax)
			time.Sleep(pause)
			continue
		}
		pause = 0
		c := h.attach(conn)
		if c == nil {
			_ = conn.Close()
			continue
		}
		go h.write(c)
		go h.readClient(c)
	}
}

func (h *Hub) readClient(c *client) {
	defer h.detach(c)
	buf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(buf)
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
