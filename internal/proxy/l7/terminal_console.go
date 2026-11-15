package l7

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/names"
)

var errBadToken = errors.New("bad console token")

// The console host has no cookie. The reason is that the site that frames the console (the catalog, the Academy) may be on another
// registrable domain than the labs, where a cookie in a cross-site frame cannot be relied on; one scheme serves a framed and a top-level
// console alike:
//
//  1. /_auth?t=<handoff> on the console host is verified exactly as for a web host (EdDSA, the tenant's key, the full host name of this
//     host, 60 s). The answer is the console page itself, not a redirect and not a Set-Cookie, with a console session token in it: the
//     proxy's own HS256 token with the claims of a session (group, client, tenant, host = this console host, abs and exp), use "console".
//  2. The page script drops ?t= from the address (history.replaceState) and opens the WebSocket with the token as the second
//     subprotocol: Sec-WebSocket-Protocol: cib.console.v1, <token>. The proxy verifies it (signature, use, exp, host equal to the request
//     host), runs the same checks as for any request (the group still belongs to the tenant, the client may reach the lab, the device has
//     a console), and answers with the first subprotocol only. The token is never logged.
//  3. Origin of the handshake: the console host itself or an origin the embedding lists allow (the frame-ancestors logic).
//  4. Renewal: there is none in the connection. The token is checked when a console opens; an open console lives until its own idle and
//     lifetime limits or until the live check cuts it. "Reconnect" on the page reuses the token the page holds while it is valid; after
//     that (or on a reload, which has no ?t=) the page shows "session over" and the user opens a new link. This is the simpler of the
//     two options (a fresh token over the socket, or a new link) and it needs no control message.
//
// The web kind keeps its host-only cookie; a console token is not a cookie of a web host and the other way round (the use claim and the host).

// ConsoleSubprotocol is the first subprotocol of the console WebSocket; the second is the console session token.
const ConsoleSubprotocol = "cib.console.v1"

// serveConsoleHost serves a console host: the handoff and the page at AuthPath, the assets, and the WebSocket at /ws. Every other path is
// the card "session over" (the page without its link) or 404.
func (h *Handler) serveConsoleHost(w http.ResponseWriter, r *http.Request, task string) {
	t := h.terminal
	ancestor := ancestorNone
	if r.URL.Path == AuthPath || r.URL.Path == "/" {
		// Only the page is framed; the answer depends on the Referer, so a cache must not reuse it.
		w.Header().Add("Vary", "Referer")
		ancestor = t.cfg.Embed.Ancestor(r.Referer())
	}
	setTerminalHeaders(w, r, ancestor)
	switch {
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		serveTerminalAsset(w, r)
	case r.URL.Path == AuthPath:
		h.consoleOpen(w, r, task)
	case r.URL.Path == "/ws":
		h.consoleSocket(w, r, task)
	case r.URL.Path == "/":
		// A reload has no ?t=: the session is over, the user opens the link again.
		h.expired(w, r)
	default:
		fail(w, r, http.StatusNotFound, pageGone, "not found")
	}
}

// consoleOpen verifies the handoff link of the console host and answers with the page that holds the console session token.
func (h *Handler) consoleOpen(w http.ResponseWriter, r *http.Request, task string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, r, http.StatusMethodNotAllowed, pageGone, "method not allowed")
		return
	}
	if !h.authLimit.allow(h.authKey(r, task)) {
		w.Header().Set("Retry-After", "5")
		fail(w, r, http.StatusTooManyRequests, pageBusy, "too many requests")
		return
	}
	raw := r.URL.Query().Get("t")
	if raw == "" || len(h.secret) == 0 {
		h.expired(w, r)
		return
	}
	now := h.now()
	host := requestHost(r)
	hc, err := verifyHandoff(raw, h.keys, h.groupTenant, host, "", h.now, h.handoffMax)
	if err != nil {
		h.expired(w, r)
		return
	}
	abs := time.Unix(hc.Session, 0)
	if limit := now.Add(h.sessionMax); h.sessionMax > 0 && abs.After(limit) {
		abs = limit
	}
	claims := jwtClaims{GroupID: hc.GroupID, Client: hc.client(), Tenant: hc.Issuer, Abs: abs.Unix(), Host: host, Use: "console",
		RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now)}}
	device, code, _ := names.ParseAccessHost(task)
	tg, ok := h.consoleTarget(w, r, claims, device, code)
	if !ok {
		return
	}
	token, _, err := h.sessionCookie(claims, now)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, pageFailed, "internal error")
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	serveTerminalPage(w, r, tg, token)
}

// consoleSocket opens the console WebSocket of a page that holds a console session token.
func (h *Handler) consoleSocket(w http.ResponseWriter, r *http.Request, task string) {
	token, ok := subprotocolToken(r.Header.Values("Sec-WebSocket-Protocol"), ConsoleSubprotocol)
	if !ok {
		h.expired(w, r)
		return
	}
	claims, err := h.verifySessionToken(token, requestHost(r), "console")
	if err != nil {
		h.expired(w, r)
		return
	}
	device, code, _ := names.ParseAccessHost(task)
	tg, ok := h.consoleTarget(w, r, claims, device, code)
	if !ok {
		return
	}
	h.terminalSocket(w, r, tg)
}

// consoleTarget runs the checks every console request passes: the group exists and still belongs to the tenant of the session, then
// authorizeTerminal (the client may reach the lab, the device has a console, its node has a service).
func (h *Handler) consoleTarget(w http.ResponseWriter, r *http.Request, claims jwtClaims, device, code string) (terminalTarget, bool) {
	group, err := h.access.Group(r.Context(), claims.GroupID)
	if err != nil || group.Tenant != claims.Tenant || claims.client() == "" {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return terminalTarget{}, false
	}
	return h.authorizeTerminal(w, r, device, code, claims, claims.client(), group)
}

// verifySessionToken checks a session token of a console or a tunnel: the proxy's signature, the use, an end in the future and the one host it was
// issued for.
func (h *Handler) verifySessionToken(raw, host, use string) (jwtClaims, error) {
	var claims jwtClaims
	token, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errBadToken
		}
		return h.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(h.now), jwt.WithExpirationRequired())
	if err != nil || !token.Valid || len(h.secret) == 0 {
		return jwtClaims{}, errBadToken
	}
	if claims.Use != use || claims.GroupID == "" || claims.Host == "" || claims.Host != host {
		return jwtClaims{}, errBadToken
	}
	return claims, nil
}

// subprotocolToken reads the offered subprotocols: exactly proto and one token, in that order.
func subprotocolToken(values []string, proto string) (string, bool) {
	var offered []string
	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				offered = append(offered, p)
			}
		}
	}
	if len(offered) != 2 || offered[0] != proto || offered[1] == proto {
		return "", false
	}
	return offered[1], true
}
