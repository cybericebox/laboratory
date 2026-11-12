package l7

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

const (
	// outputWindow is how many output bytes may be on their way to the browser without an acknowledgement. The browser acknowledges
	// what it has drawn; past the window the proxy stops reading from the service, so a browser that cannot keep up slows the shell
	// down instead of filling a buffer here.
	outputWindow = 512 << 10
	// ackStall is how long the window may stay full before the console is closed as stalled.
	ackStall = 60 * time.Second
	// terminalWrite bounds every write towards the browser or the service.
	terminalWrite = 30 * time.Second
	// terminalHello bounds the wait for the first message of the browser.
	terminalHello = 10 * time.Second
)

type hello struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type browserMsg struct {
	Type    string `json:"type"`
	Session string `json:"session,omitempty"`
	Code    string `json:"code,omitempty"`
}

func browserText(ctx context.Context, c *websocket.Conn, m browserMsg) error {
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(ctx, terminalWrite)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

// terminalSocket is the console's WebSocket: the browser's stream relayed to the service on the node of the device.
func (h *Handler) terminalSocket(w http.ResponseWriter, r *http.Request, tg terminalTarget) {
	t := h.terminal
	if !t.sameOrigin(r) {
		// A page of a device, or any other site, may not drive the console with the participant's cookie.
		fail(w, r, http.StatusForbidden, pageGone, "forbidden")
		return
	}
	token, err := newToken()
	if err != nil {
		fail(w, r, http.StatusInternalServerError, pageFailed, "internal error")
		return
	}
	// Registered as a live request: when the policy denies the client or the lab stops, CheckLive closes it within seconds.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	entry := &liveEntry{group: tg.claims.GroupID, client: tg.client, lab: tg.lab, tenant: tg.claims.Tenant, deadline: h.liveDeadline(tg.claims.Abs), cancel: cancel}
	if !h.live.tryAdd(entry, LiveCaps{}) {
		fail(w, r, http.StatusServiceUnavailable, pageBusy, "service unavailable")
		return
	}
	defer h.live.remove(entry)

	sess := &consoleSession{token: token, group: tg.claims.GroupID, client: tg.client, tenant: tg.claims.Tenant, namespace: tg.namespace, lab: tg.lab, dev: tg.device, host: tg.host,
		base: tg.base, command: tg.info.Command, user: tg.info.User, entry: entry, ackCh: make(chan struct{}, 1), started: time.Now()}
	sess.budget.Store(t.cfg.MaxSession)
	if !t.add(sess) {
		fail(w, r, http.StatusTooManyRequests, pageBusy, "too many consoles")
		return
	}
	defer t.remove(sess)

	browser, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	browser.SetReadLimit(wire.MaxFrame)
	defer func() { _ = browser.CloseNow() }()

	reason := h.relayConsole(ctx, browser, sess, entry)
	// The console is over for everything that looks it up before the browser has even acknowledged the close.
	t.remove(sess)
	t.cfg.Log.Info("console end", "reason", reason, "lab", sess.lab, "device", sess.dev, "namespace", sess.namespace, "client", sess.client, "group", sess.group,
		"user", sess.user, "commandHash", wire.CommandHash(sess.command), "duration", time.Since(sess.started).Round(time.Millisecond).String(), "bytesIn", sess.bytesIn.Load(), "bytesOut", sess.bytesOut.Load())
	_ = browser.Close(websocket.StatusNormalClosure, reason)
}

// relayConsole runs the whole life of a console and returns why it ended.
func (h *Handler) relayConsole(ctx context.Context, browser *websocket.Conn, sess *consoleSession, entry *liveEntry) string {
	t := h.terminal
	// The first message is the size of the browser's terminal.
	hctx, hcancel := context.WithTimeout(ctx, terminalHello)
	typ, msg, err := browser.Read(hctx)
	hcancel()
	var hi hello
	if err != nil || typ != websocket.MessageText || json.Unmarshal(msg, &hi) != nil || hi.Cols < wire.MinSize || hi.Cols > wire.MaxSize || hi.Rows < wire.MinSize || hi.Rows > wire.MaxSize {
		_ = browserText(ctx, browser, browserMsg{Type: "error", Code: wire.CodeBadRequest})
		return wire.ReasonError
	}

	svc, err := t.dial(ctx, sess.base)
	if err != nil {
		_ = browserText(ctx, browser, browserMsg{Type: "error", Code: wire.CodeUnavailable})
		return wire.ReasonError
	}
	defer func() { _ = svc.CloseNow() }()
	open, err := t.openMessage(wire.OpenRequest{Kind: wire.KindConsole, Namespace: sess.namespace, Lab: sess.lab, Device: sess.dev,
		Command: sess.command, User: sess.user, Cols: hi.Cols, Rows: hi.Rows, Client: sess.client, Group: sess.group})
	if err != nil {
		_ = browserText(ctx, browser, browserMsg{Type: "error", Code: wire.CodeUnavailable})
		return wire.ReasonError
	}
	wctx, wcancel := context.WithTimeout(ctx, terminalWrite)
	err = svc.Write(wctx, websocket.MessageText, open)
	wcancel()
	if err != nil {
		_ = browserText(ctx, browser, browserMsg{Type: "error", Code: wire.CodeUnavailable})
		return wire.ReasonError
	}
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
	rt, rmsg, err := svc.Read(rctx)
	rcancel()
	var reply wire.OpenReply
	if err != nil || rt != websocket.MessageText || json.Unmarshal(rmsg, &reply) != nil {
		_ = browserText(ctx, browser, browserMsg{Type: "error", Code: wire.CodeUnavailable})
		return wire.ReasonError
	}
	if !reply.OK {
		_ = browserText(ctx, browser, browserMsg{Type: "error", Code: browserCode(reply.Error)})
		return wire.ReasonRefused
	}
	if err := browserText(ctx, browser, browserMsg{Type: "ready", Session: sess.token}); err != nil {
		return wire.ReasonError
	}

	rel, relCancel := context.WithCancel(ctx)
	defer relCancel()
	sess.touch()
	done := make(chan string, 2)
	go func() { done <- h.relayUp(rel, browser, svc, sess) }()
	go func() { done <- h.relayDown(rel, browser, svc, sess) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var life <-chan time.Time
	if t.cfg.Lifetime > 0 {
		timer := time.NewTimer(t.cfg.Lifetime)
		defer timer.Stop()
		life = timer.C
	}
	var reason string
loop:
	for {
		select {
		case reason = <-done:
			break loop
		case <-life:
			reason = wire.ReasonLifetime
			break loop
		case <-tick.C:
			if t.cfg.Idle > 0 && time.Since(time.Unix(0, sess.lastActive.Load())) > t.cfg.Idle {
				reason = wire.ReasonIdle
				break loop
			}
		case <-ctx.Done():
			reason = wire.ReasonAccess
			if !entry.deadline.IsZero() && !time.Now().Before(entry.deadline) {
				reason = wire.ReasonLifetime
			} else if h.live.stopping() {
				reason = wire.ReasonShutdown
			}
			break loop
		}
	}
	// Tell both sides why; the service removes the shell when its stream ends. This goes out before the relay contexts are cancelled:
	// cancelling the context of a WebSocket read closes the connection.
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ccancel()
	_ = svc.Write(cctx, websocket.MessageBinary, wire.Close(reason))
	_ = browser.Write(cctx, websocket.MessageBinary, wire.Close(reason))
	relCancel()
	return reason
}

func browserCode(c string) string {
	switch c {
	case wire.CodeNotFound, wire.CodeDisabled, wire.CodeLimit, wire.CodeBadRequest, wire.CodeUnavailable:
		return c
	}
	return wire.CodeUnavailable
}

func (s *consoleSession) touch() { s.lastActive.Store(time.Now().UnixNano()) }

// relayUp carries the browser's frames to the service. Every frame is parsed first; the service never sees a message the proxy has
// not checked.
func (h *Handler) relayUp(ctx context.Context, browser, svc *websocket.Conn, sess *consoleSession) string {
	for {
		typ, msg, err := browser.Read(ctx)
		if err != nil {
			return wire.ReasonClient
		}
		if typ != websocket.MessageBinary {
			return wire.ReasonError
		}
		ft, payload, perr := wire.Parse(msg)
		if perr != nil {
			return wire.ReasonError
		}
		switch ft {
		case wire.TypeAck:
			sess.acked.Add(int64(wire.AckValue(payload)))
			select {
			case sess.ackCh <- struct{}{}:
			default:
			}
			continue
		case wire.TypeClose:
			return wire.ReasonClient
		case wire.TypeData:
			sess.bytesIn.Add(int64(len(payload)))
		}
		sess.touch()
		wctx, cancel := context.WithTimeout(ctx, terminalWrite)
		err = svc.Write(wctx, websocket.MessageBinary, msg)
		cancel()
		if err != nil {
			return wire.ReasonError
		}
	}
}

// relayDown carries the service's frames to the browser, holding back when the browser has not acknowledged enough.
func (h *Handler) relayDown(ctx context.Context, browser, svc *websocket.Conn, sess *consoleSession) string {
	for {
		typ, msg, err := svc.Read(ctx)
		if err != nil {
			return wire.ReasonError
		}
		if typ != websocket.MessageBinary {
			return wire.ReasonError
		}
		ft, payload, perr := wire.Parse(msg)
		if perr != nil {
			return wire.ReasonError
		}
		switch ft {
		case wire.TypeClose:
			return string(payload)
		case wire.TypeData:
			if !sess.waitWindow(ctx, int64(len(payload))) {
				if ctx.Err() != nil {
					return wire.ReasonClient
				}
				return wire.ReasonStalled
			}
			sess.bytesOut.Add(int64(len(payload)))
			sess.touch()
		default:
			return wire.ReasonError
		}
		wctx, cancel := context.WithTimeout(ctx, terminalWrite)
		err = browser.Write(wctx, websocket.MessageBinary, msg)
		cancel()
		if err != nil {
			return wire.ReasonClient
		}
	}
}

// waitWindow reserves n bytes of the output window, waiting for acknowledgements while it is full. It reports false when the
// console ended or the window stayed full for ackStall.
func (s *consoleSession) waitWindow(ctx context.Context, n int64) bool {
	deadline := time.NewTimer(ackStall)
	defer deadline.Stop()
	for s.sent.Load()-s.acked.Load()+n > outputWindow {
		select {
		case <-s.ackCh:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	s.sent.Add(n)
	return true
}
