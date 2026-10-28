package l7

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

// pacer holds the traffic of one participant (a client of a group) to its fair share of the entry class of this proxy: every read and
// write of a request waits for the time the bytes cost (the stream is paced, not cut). The participant is the same across requests
// and connections, so ten tabs of one person share one share.
type pacer struct {
	th  *bandwidth.Throttle[string]
	key string
	ctx context.Context
}

// wait pays for n bytes; it returns early with the context's error when the request is cancelled.
func (p *pacer) wait(n int) error {
	if p == nil || n <= 0 {
		return nil
	}
	d := p.th.Delay(p.key, n)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// pacedConn paces an upgraded (WebSocket) connection in both directions.
type pacedConn struct {
	net.Conn
	r    io.Reader // the connection as the server had read it so far: its buffered bytes first
	pace *pacer
}

func (c *pacedConn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if werr := c.pace.wait(n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}

func (c *pacedConn) Write(b []byte) (int, error) {
	if err := c.pace.wait(len(b)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// Throttle is the throttle of the proxy: SetLimits turns the pacing on with the class of the proxy and the ceiling of one participant.
func (h *Handler) Throttle() *bandwidth.Throttle[string] { return h.throttle }

// pacerFor makes the pacer of a request, nil when the throttle has no limits (the common case costs nothing).
func (h *Handler) pacerFor(ctx context.Context, group, client string) *pacer {
	if h.throttle == nil || !h.throttle.Enabled() {
		return nil
	}
	return &pacer{th: h.throttle, key: group + "|" + client, ctx: ctx}
}
