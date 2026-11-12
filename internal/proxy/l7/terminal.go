package l7

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The console. A device whose lab asks for a terminal can be opened in the browser on a reserved host of the labs' base domain,
// terminal.<base>: the page, a WebSocket for the shell and two requests for files all live there. The proxy authorizes exactly as it
// does for a web request (the session cookie, the group, the access policy that allows the lab, the lab running) and adds that the
// device has a console. It then relays the stream to the device access service on the node of the device, which opens the shell.
// The proxy never learns the container; it names a device (namespace, lab, device) and the service finds the container itself.
//
// Because the cookie of the session covers every host under the base domain, a page of a device (written by a participant) could ask
// for the console of the participant's own session. What stops it: the WebSocket accepts only the page's own Origin, and a file
// request needs the token that only this page receives over the WebSocket.

// ErrNoConsole is returned by a DeviceLookup for a device that has no console (or does not exist; the answer is the same).
var ErrNoConsole = errors.New("the device has no console")

// DeviceInfo is what the proxy needs to open a console: the node the device runs on and the terminal settings the lab author gave.
type DeviceInfo struct {
	Node    string
	Command []string
	User    string
}

// DeviceLookup finds a device with a console in a group namespace.
type DeviceLookup func(ctx context.Context, namespace, lab, device string) (DeviceInfo, error)

// ServiceDirectory maps a node to the base URL of the device access service pod on it, e.g. "http://10.0.3.7:8080".
type ServiceDirectory func(node string) (baseURL string, ok bool)

// DefaultTerminalHost is the reserved first label of the console's host.
const DefaultTerminalHost = "terminal"

// TerminalConfig switches the console on in a Handler.
type TerminalConfig struct {
	// Host is the first label of the host of the console (default "terminal").
	Host     string
	Devices  DeviceLookup
	Services ServiceDirectory
	Log      logr.Logger
	// PerClient bounds the consoles one client of a group has open through this proxy. 0 is no limit.
	PerClient int
	// Idle closes a console that no byte crossed for that long; Lifetime closes any console at that age. 0 is no limit.
	Idle, Lifetime time.Duration
	// MaxFile is the largest file one transfer moves; MaxSession is the most one console moves in all (both directions).
	MaxFile, MaxSession int64
	// OriginScheme is the scheme of the page's origin ("https" unless a test serves it plainly).
	OriginScheme string
	// Dial opens the stream to a service; nil dials the base URL of the directory over a WebSocket.
	Dial func(ctx context.Context, base string) (*websocket.Conn, error)
}

// DefaultTerminalConfig carries the chart defaults for the limits.
func DefaultTerminalConfig() TerminalConfig {
	return TerminalConfig{Host: DefaultTerminalHost, PerClient: 3, Idle: 30 * time.Minute, Lifetime: 8 * time.Hour,
		MaxFile: 16 << 20, MaxSession: 64 << 20, OriginScheme: "https"}
}

type terminalState struct {
	cfg TerminalConfig

	mu        sync.Mutex
	sessions  map[string]*consoleSession
	perClient map[liveClientKey]int
}

// consoleSession is an open console, found by the token the page received.
type consoleSession struct {
	token               string
	group, client       string
	tenant              string
	namespace, lab, dev string
	base                string
	command             []string
	user                string
	entry               *liveEntry
	// budget is what the console may still move as files; transferring is set while a transfer runs (one at a time).
	budget       atomic.Int64
	transferring atomic.Bool
	// sent and acked count output bytes towards the browser and the ones it has drawn (the flow control, see relayDown).
	sent, acked       atomic.Int64
	ackCh             chan struct{}
	started           time.Time
	lastActive        atomic.Int64
	bytesIn, bytesOut atomic.Int64
}

// WithTerminal switches the console on. It needs the access reader: without it the proxy has no groups to authorize a console by.
func (h *Handler) WithTerminal(c TerminalConfig) *Handler {
	if c.Host == "" {
		c.Host = DefaultTerminalHost
	}
	if c.OriginScheme == "" {
		c.OriginScheme = "https"
	}
	h.terminal = &terminalState{cfg: c, sessions: map[string]*consoleSession{}, perClient: map[liveClientKey]int{}}
	return h
}

func (t *terminalState) add(s *consoleSession) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := liveClientKey{s.group, s.client}
	if t.cfg.PerClient > 0 && t.perClient[key] >= t.cfg.PerClient {
		return false
	}
	t.perClient[key]++
	t.sessions[s.token] = s
	return true
}

func (t *terminalState) remove(s *consoleSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sessions[s.token]; !ok {
		return
	}
	delete(t.sessions, s.token)
	key := liveClientKey{s.group, s.client}
	if t.perClient[key]--; t.perClient[key] <= 0 {
		delete(t.perClient, key)
	}
}

func (t *terminalState) session(token string) *consoleSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[token]
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// terminalTarget is a console request that passed authorization.
type terminalTarget struct {
	namespace, lab, device string
	claims                 jwtClaims
	client                 string
	info                   DeviceInfo
	base                   string
}

// serveTerminal answers a request for the console host. The session was validated by the caller (cookie, group, tenant).
func (h *Handler) serveTerminal(w http.ResponseWriter, r *http.Request, claims jwtClaims, client string, group AccessGroup) {
	setTerminalHeaders(w, r)
	switch {
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		serveTerminalAsset(w, r)
	case r.URL.Path == "/":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, r, http.StatusMethodNotAllowed, pageGone, "method not allowed")
			return
		}
		tg, ok := h.authorizeTerminal(w, r, claims, client, group)
		if !ok {
			return
		}
		serveTerminalPage(w, r, tg)
	case r.URL.Path == "/ws":
		tg, ok := h.authorizeTerminal(w, r, claims, client, group)
		if !ok {
			return
		}
		h.terminalSocket(w, r, tg, group)
	case r.URL.Path == "/files":
		h.terminalFile(w, r, claims, client, group)
	default:
		fail(w, r, http.StatusNotFound, pageGone, "not found")
	}
}

// authorizeTerminal is the authorization of a console request: the client may reach the lab (the same Allowed check as for a web
// request: the client exists, the policy allows the lab, the lab is not stopped) and the device has a console. Every refusal is the
// same card, so the answer does not tell a lab that does not exist from one the client may not use.
func (h *Handler) authorizeTerminal(w http.ResponseWriter, r *http.Request, claims jwtClaims, client string, group AccessGroup) (terminalTarget, bool) {
	t := h.terminal
	lab, device := r.URL.Query().Get("lab"), r.URL.Query().Get("device")
	if !plainName(lab) || !plainName(device) || t.cfg.Devices == nil || t.cfg.Services == nil {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return terminalTarget{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !h.access.Allowed(ctx, group.Namespace, client, lab) {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return terminalTarget{}, false
	}
	info, err := t.cfg.Devices(ctx, group.Namespace, lab, device)
	if err != nil {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return terminalTarget{}, false
	}
	base, ok := t.cfg.Services(info.Node)
	if !ok {
		// The device is not placed yet, or its node has no service: not an error of the client.
		fail(w, r, http.StatusServiceUnavailable, pageStarting, "the console is not ready")
		return terminalTarget{}, false
	}
	return terminalTarget{namespace: group.Namespace, lab: lab, device: device, claims: claims, client: client, info: info, base: base}, true
}

// plainName accepts a name that can be a lab or device: bounded, printable, no separator or control character.
func plainName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e || c == '/' || c == '\\' || c == '"' || c == '<' || c == '>' || c == '&' {
			return false
		}
	}
	return true
}

// setTerminalHeaders are the headers of every answer of the console host. The page may run its own scripts and styles and open a
// WebSocket to its own host, and nothing else: no frame, no form, no base tag, no external host. The stylesheet needs 'unsafe-inline'
// because the terminal library writes style elements; scripts stay strict.
func setTerminalHeaders(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	hd := w.Header()
	hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' wss://"+host+
		"; img-src 'self' data:; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cache-Control", "no-store")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// sameOrigin reports whether the Origin header is the page's own origin. A request without Origin is not (a browser sends one on a
// WebSocket handshake and on a cross-origin or non-GET request).
func (t *terminalState) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o != "" && strings.EqualFold(o, t.cfg.OriginScheme+"://"+r.Host)
}

// defaultDial opens the stream to a service.
func defaultDial(ctx context.Context, base string) (*websocket.Conn, error) {
	url := "ws" + strings.TrimPrefix(base, "http") + wire.StreamPath
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	return c, err
}

func (t *terminalState) dial(ctx context.Context, base string) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dial := t.cfg.Dial
	if dial == nil {
		dial = defaultDial
	}
	c, err := dial(ctx, base)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(wire.MaxFrame)
	return c, nil
}
