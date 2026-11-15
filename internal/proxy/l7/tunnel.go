package l7

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
)

// The gRPC-kind access of a device (EDA web-direct, programmatic level): a byte tunnel over a WebSocket. The proxy does not touch the
// application protocol (gRPC, TLS, mTLS: whatever the client and the device speak passes as bytes), so any authentication of the device
// service works through it, end-to-end mTLS included.
//
//	POST /_access/exchange  {"handoff": "<jwt>"}   -> {"token", "expires_at", "max_until"}
//	POST /_access/refresh   {"token": "<token>"}    -> the same, never past max_until
//	GET  /_tunnel           Upgrade: websocket, Sec-WebSocket-Protocol: cib.tunnel.v1, <token>
//
// The handoff is the 60 s link of the platform for this host (full host name, use "tunnel"); the token is the proxy's own HS256 token (same
// secret as the console and the session cookie, use "tunnel", host = this host, group, tenant, the key id, an idle limit and an absolute
// limit). After the upgrade, binary messages are the bytes of a TCP connection to the Service port of the device. The proxy echoes only
// cib.tunnel.v1, checks while the tunnel is open (the group still belongs to the tenant, the lab runs, the device still has the kind, the
// tenant key is still registered) and closes it when a check fails, meters and paces the bytes like web traffic, and never logs a token.
// A request with an Origin header is refused on every path: a browser must not be able to use it.

// TunnelSubprotocol is the first subprotocol of the tunnel WebSocket; the second is the token.
const TunnelSubprotocol = "cib.tunnel.v1"

const (
	tunnelUse = "tunnel"
	// DefaultTunnelTokenTTL is how long a tunnel token is valid without a refresh; the absolute limit (max_until) is the link's session end,
	// never past the session maximum.
	DefaultTunnelTokenTTL = time.Hour
	tunnelDialTimeout     = 5 * time.Second
	tunnelCopyBuffer      = 32 * 1024
	tunnelReadLimit       = 1 << 20
	tunnelBodyLimit       = 8 << 10
)

type tunnelConfig struct {
	ttl  time.Duration
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// WithTunnelTokenTTL sets how long a tunnel token is valid without a refresh (zero: DefaultTunnelTokenTTL).
func (h *Handler) WithTunnelTokenTTL(d time.Duration) *Handler {
	h.tunnel.ttl = d
	return h
}

// WithTunnelDial replaces the TCP dial to the device (tests).
func (h *Handler) WithTunnelDial(dial func(ctx context.Context, addr string) (net.Conn, error)) *Handler {
	h.tunnel.dial = dial
	return h
}

// TunnelToken is the answer of the exchange and the refresh.
type TunnelToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxUntil  time.Time `json:"max_until"`
}

func tunnelError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// serveTunnelHost serves a host of the gRPC kind.
func (h *Handler) serveTunnelHost(w http.ResponseWriter, r *http.Request, task string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("Origin") != "" {
		// Not for browsers: a page, of a device or of anyone, cannot use the tunnel or its token endpoints.
		tunnelError(w, http.StatusForbidden, "forbidden")
		return
	}
	switch {
	case r.URL.Path == "/_access/exchange" && r.Method == http.MethodPost:
		h.tunnelExchange(w, r, task)
	case r.URL.Path == "/_access/refresh" && r.Method == http.MethodPost:
		h.tunnelRefresh(w, r, task)
	case r.URL.Path == "/_tunnel" && r.Method == http.MethodGet:
		h.tunnelSocket(w, r, task)
	case r.URL.Path == "/_access/exchange" || r.URL.Path == "/_access/refresh" || r.URL.Path == "/_tunnel":
		tunnelError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	default:
		tunnelError(w, http.StatusNotFound, "not_found")
	}
}

func (h *Handler) tunnelRateOK(w http.ResponseWriter, r *http.Request, task string) bool {
	if h.authLimit.allow(h.authKey(r, task)) {
		return true
	}
	w.Header().Set("Retry-After", "5")
	tunnelError(w, http.StatusTooManyRequests, "too_many_requests")
	return false
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, tunnelBodyLimit)
	return json.NewDecoder(r.Body).Decode(into) == nil
}

// tunnelExchange turns the platform's 60 s link for this host into a tunnel token.
func (h *Handler) tunnelExchange(w http.ResponseWriter, r *http.Request, task string) {
	if !h.tunnelRateOK(w, r, task) {
		return
	}
	var in struct {
		Handoff string `json:"handoff"`
	}
	if !readJSON(w, r, &in) || in.Handoff == "" || len(h.secret) == 0 {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	now := h.now()
	host := requestHost(r)
	hc, err := verifyHandoff(in.Handoff, h.keys, h.groupTenant, host, tunnelUse, h.now, h.handoffMax)
	if err != nil {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	abs := time.Unix(hc.Session, 0)
	if limit := now.Add(h.sessionMax); h.sessionMax > 0 && abs.After(limit) {
		abs = limit
	}
	claims := jwtClaims{GroupID: hc.GroupID, Client: hc.client(), Tenant: hc.Issuer, Host: host, Use: tunnelUse, Kid: hc.Kid, Abs: abs.Unix()}
	if _, _, ok := h.tunnelAllowed(r.Context(), task, claims); !ok {
		tunnelError(w, http.StatusNotFound, "not_found")
		return
	}
	h.issueTunnelToken(w, claims, now)
}

// tunnelRefresh gives a token for a token that is still valid, never past its absolute limit.
func (h *Handler) tunnelRefresh(w http.ResponseWriter, r *http.Request, task string) {
	if !h.tunnelRateOK(w, r, task) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &in) || in.Token == "" {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	claims, err := h.verifySessionToken(in.Token, requestHost(r), tunnelUse)
	if err != nil || !h.now().Before(time.Unix(claims.Abs, 0)) {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, _, ok := h.tunnelAllowed(r.Context(), task, claims); !ok {
		tunnelError(w, http.StatusNotFound, "not_found")
		return
	}
	claims.RegisteredClaims = jwt.RegisteredClaims{}
	h.issueTunnelToken(w, claims, h.now())
}

func (h *Handler) issueTunnelToken(w http.ResponseWriter, claims jwtClaims, now time.Time) {
	ttl := h.tunnel.ttl
	if ttl <= 0 {
		ttl = DefaultTunnelTokenTTL
	}
	abs := time.Unix(claims.Abs, 0)
	end := now.Add(ttl)
	if end.After(abs) {
		end = abs
	}
	if !end.After(now) {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	claims.IssuedAt = jwt.NewNumericDate(now)
	claims.ExpiresAt = jwt.NewNumericDate(end)
	value, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(h.secret)
	if err != nil {
		tunnelError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(TunnelToken{Token: value, ExpiresAt: end.UTC(), MaxUntil: abs.UTC()})
}

// tunnelAllowed is what every tunnel request is judged by: the group exists and still belongs to the tenant of the session, the tenant key
// that opened the session is still registered, the host is the gRPC-kind Service of a device of the group and its lab runs.
func (h *Handler) tunnelAllowed(ctx context.Context, task string, claims jwtClaims) (AccessRoute, AccessGroup, bool) {
	if _, ok := h.keys(claims.Tenant, claims.Kid); !ok {
		return AccessRoute{}, AccessGroup{}, false
	}
	group, err := h.access.Group(ctx, claims.GroupID)
	if err != nil || group.Tenant != claims.Tenant {
		return AccessRoute{}, AccessGroup{}, false
	}
	route, err := h.access.TunnelRoute(ctx, task, group.Namespace)
	if err != nil || !h.access.LabRunning(ctx, group.Namespace, route.Lab) {
		return AccessRoute{}, AccessGroup{}, false
	}
	return route, group, true
}

// tunnelSocket opens the tunnel.
func (h *Handler) tunnelSocket(w http.ResponseWriter, r *http.Request, task string) {
	token, ok := subprotocolToken(r.Header.Values("Sec-WebSocket-Protocol"), TunnelSubprotocol)
	if !ok {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	claims, err := h.verifySessionToken(token, requestHost(r), tunnelUse)
	if err != nil {
		tunnelError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	route, group, ok := h.tunnelAllowed(r.Context(), task, claims)
	if !ok {
		tunnelError(w, http.StatusNotFound, "not_found")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	tenant, kid, ns := claims.Tenant, claims.Kid, group.Namespace
	entry := &liveEntry{group: claims.GroupID, client: claims.Client, lab: route.Lab, tenant: tenant, deadline: h.liveDeadline(claims.Abs), cancel: cancel,
		runningOnly: true,
		valid: func() bool {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, ok := h.keys(tenant, kid); !ok {
				return false
			}
			_, err := h.access.TunnelRoute(c, task, ns)
			return err == nil
		}}
	if !h.live.tryAdd(entry, h.caps) {
		w.Header().Set("Retry-After", "5")
		tunnelError(w, http.StatusTooManyRequests, "too_many_requests")
		return
	}
	defer h.live.remove(entry)

	dialCtx, dialCancel := context.WithTimeout(ctx, tunnelDialTimeout)
	dial := h.tunnel.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	}
	device, err := dial(dialCtx, route.URL)
	dialCancel()
	if err != nil {
		tunnelError(w, http.StatusBadGateway, "device_unavailable")
		return
	}
	defer device.Close()

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{TunnelSubprotocol}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(tunnelReadLimit)
	defer func() { _ = ws.CloseNow() }()

	var request *RequestMeter
	if h.meter != nil && route.Lab != "" {
		request = h.meter.Begin(ns, claims.Client, route.Lab, h.now())
		defer request.End()
		request.Responded(h.now())
	}
	pace := h.pacerFor(ctx, ns, claims.Client)
	h.relayTunnel(ctx, r.Context(), ws, device, request, pace)
}

// relayTunnel copies bytes both ways until one side ends, the access is cut (ctx) or the client goes (clientCtx), then says why with the
// close status of the WebSocket (going away on a stop of the service, a policy violation when the access ended) and ends the other side.
// Bytes from the client to the device are counted as the out side of the request, bytes back as the in side, like the body and the
// response of a web request.
func (h *Handler) relayTunnel(ctx, clientCtx context.Context, ws *websocket.Conn, device net.Conn, request *RequestMeter, pace *pacer) {
	client := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	var wg sync.WaitGroup
	done := make(chan struct{}, 2)
	copyOne := func(dst io.Writer, src io.Reader, count func(int64)) {
		defer wg.Done()
		defer func() { done <- struct{}{} }()
		buf := make([]byte, tunnelCopyBuffer)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if werr := pace.wait(n); werr != nil {
					return
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					if request != nil {
						request.Incomplete()
					}
					return
				}
				if count != nil {
					count(int64(n))
				}
			}
			if err != nil {
				return
			}
		}
	}
	var out, in func(int64)
	if request != nil {
		out, in = request.AddOut, request.AddIn
	}
	wg.Add(2)
	go copyOne(device, client, out)
	go copyOne(client, device, in)
	select {
	case <-done:
	case <-ctx.Done():
	}
	status, reason := websocket.StatusNormalClosure, "closed"
	switch {
	case h.live.stopping():
		status, reason = websocket.StatusGoingAway, "service restarting"
	case ctx.Err() != nil && clientCtx.Err() == nil:
		status, reason = websocket.StatusPolicyViolation, "access ended"
	}
	_ = device.Close()
	_ = ws.Close(status, reason)
	wg.Wait()
}
