package l7

import (
	"net/http"
	"strings"

	"github.com/cybericebox/laboratory/internal/names"
)

// The console host has no cookie. The reason is that the site that frames the console (the catalog, the Academy) may be on another
// registrable domain than the labs, where a cookie in a cross-site frame cannot be relied on; one scheme serves a framed and a top-level
// console alike:
//
//  1. POST /_auth on the console host carries a console grant (see session.go: the same form, the same Origin check). The grant is verified
//     for this host (EdDSA, the tenant's key, the full host name, kind "console", aud laboratory-proxy). The answer is the console page itself,
//     not a redirect and not a Set-Cookie, with the grant in a JSON block of the page. There is nothing in the URL.
//  2. The page script opens the WebSocket with the grant as the second subprotocol: Sec-WebSocket-Protocol: cib.console.v1, <grant>. The
//     proxy verifies it again, runs the same checks as for any request (the group still belongs to the tenant, the client may reach the lab,
//     the device has a console), and answers with the first subprotocol only. The grant is never logged. Towards the device access service
//     the proxy forwards the same grant in the open request; the service verifies it on the node.
//  3. Origin of the handshake: the console host itself or an origin the embedding lists allow (the frame-ancestors logic).
//  4. Renewal: there is none in the connection. The grant is checked when a console opens and the stream ends at its exp. "Reconnect" on
//     the page reuses the grant the page holds while it is valid; after that (or on a reload, which holds no grant) the page shows
//     "session over" and the user opens the device again.
//
// The web kind keeps its host-only cookie; a console grant is not a cookie of a web host and the other way round (the kind and the host).

// ConsoleSubprotocol is the first subprotocol of the console WebSocket; the second is the console session token.
const ConsoleSubprotocol = "cib.console.v1"

// serveConsoleHost serves a console host: the grant post and the page at AuthPath, the assets, and the WebSocket at /ws. Every other path is
// the card "session over" or 404.
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
		// A reload holds no grant: the session is over, the user opens the device again.
		h.expired(w, r)
	default:
		fail(w, r, http.StatusNotFound, pageGone, "not found")
	}
}

// consoleOpen verifies the console grant posted to the console host and answers with the page that holds it.
func (h *Handler) consoleOpen(w http.ResponseWriter, r *http.Request, task string) {
	if !h.throttlePreAuth(w, r) {
		return
	}
	raw, _, ok := h.readAuthForm(w, r)
	if !ok {
		return
	}
	if !h.acquireVerify(w, r) {
		return
	}
	g, err := h.verifyLogin(raw, task, requestHost(r))
	h.releaseVerify()
	if err != nil {
		h.expired(w, r)
		return
	}
	if !h.throttleHandoff(w, r, task, g.Group+"|"+g.client()) {
		return
	}
	device, code, _ := names.ParseAccessHost(task)
	tg, ok := h.consoleTarget(w, r, g, device, code)
	if !ok {
		return
	}
	serveTerminalPage(w, r, tg, raw)
}

// consoleSocket opens the console WebSocket of a page that holds a console session token.
func (h *Handler) consoleSocket(w http.ResponseWriter, r *http.Request, task string) {
	raw, ok := subprotocolToken(r.Header.Values("Sec-WebSocket-Protocol"), ConsoleSubprotocol)
	if !ok {
		h.expired(w, r)
		return
	}
	// The grant on the socket is verified like the one on /_auth: an Ed25519 verification costs CPU, so a garbage flood is throttled per address
	// before the grant is looked at and the verifications run in a bounded number of slots.
	if !h.throttlePreAuth(w, r) {
		return
	}
	if !h.acquireVerify(w, r) {
		return
	}
	claims, err := h.verifyGrant(raw, task, requestHost(r))
	h.releaseVerify()
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
func (h *Handler) consoleTarget(w http.ResponseWriter, r *http.Request, claims sessionGrant, device, code string) (terminalTarget, bool) {
	group, err := h.access.Group(r.Context(), claims.Group)
	if err != nil || group.Tenant != claims.Tenant() || claims.client() == "" {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return terminalTarget{}, false
	}
	return h.authorizeTerminal(w, r, device, code, claims, claims.client(), group)
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
