package labtunnel

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// writeChunk bounds one WebSocket message; a larger Write is sent in pieces (the stream has no message boundaries).
const writeChunk = 64 * 1024

type tunnelAddr string

func (tunnelAddr) Network() string  { return "labtunnel" }
func (a tunnelAddr) String() string { return string(a) }

// conn is a net.Conn over the WebSocket of a tunnel. websocket.NetConn closes the connection when a deadline expires and reports it as a
// cancelled context, which is not what a net.Conn promises (a caller of gRPC or TLS retries after a timeout), so this one keeps a reader
// goroutine and answers a read deadline with os.ErrDeadlineExceeded (a net.Error with Timeout() true) and leaves the connection alone. A
// write that does not finish by its deadline fails the connection, as a stuck write must.
type conn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	local  tunnelAddr
	remote tunnelAddr

	in      chan []byte
	rerr    error // set before in is closed
	pending []byte

	mu     sync.Mutex
	rdl    time.Time
	wdl    time.Time
	wake   chan struct{} // a read deadline changed
	closed bool
	wmu    sync.Mutex
}

func newConn(ws *websocket.Conn, remote string) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{ws: ws, ctx: ctx, cancel: cancel, local: "labtunnel", remote: tunnelAddr(remote), in: make(chan []byte, 1), wake: make(chan struct{}, 1)}
	go c.pump()
	return c
}

// pump reads the messages of the tunnel for ever, so a read deadline never has to interrupt a read of the WebSocket.
func (c *conn) pump() {
	defer close(c.in)
	for {
		typ, msg, err := c.ws.Read(c.ctx)
		if err != nil {
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				c.rerr = io.EOF
			default:
				c.rerr = err
			}
			return
		}
		if typ != websocket.MessageBinary {
			c.rerr = io.ErrUnexpectedEOF
			return
		}
		select {
		case c.in <- msg:
		case <-c.ctx.Done():
			c.rerr = net.ErrClosed
			return
		}
	}
}

func (c *conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(c.pending) > 0 {
			n := copy(p, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}
		c.mu.Lock()
		closed, dl := c.closed, c.rdl
		c.mu.Unlock()
		if closed {
			return 0, net.ErrClosed
		}
		var timer <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			t := time.NewTimer(d)
			defer t.Stop()
			timer = t.C
		}
		select {
		case msg, ok := <-c.in:
			if !ok {
				c.mu.Lock()
				closed = c.closed
				c.mu.Unlock()
				if closed {
					return 0, net.ErrClosed
				}
				return 0, c.rerr
			}
			c.pending = msg
		case <-timer:
			return 0, os.ErrDeadlineExceeded
		case <-c.wake:
			// the deadline changed: look again
		}
	}
}

func (c *conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	for len(p) > 0 {
		c.mu.Lock()
		closed, dl := c.closed, c.wdl
		c.mu.Unlock()
		if closed {
			return written, net.ErrClosed
		}
		ctx, cancel := c.ctx, context.CancelFunc(func() {})
		if !dl.IsZero() {
			if !time.Now().Before(dl) {
				return written, os.ErrDeadlineExceeded
			}
			ctx, cancel = context.WithDeadline(c.ctx, dl)
		}
		n := min(len(p), writeChunk)
		err := c.ws.Write(ctx, websocket.MessageBinary, p[:n])
		cancel()
		if err != nil {
			if !dl.IsZero() && !time.Now().Before(dl) {
				return written, os.ErrDeadlineExceeded
			}
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	c.cancel()
	_ = c.ws.CloseNow()
	return err
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

func (c *conn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl = t
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return nil
}
