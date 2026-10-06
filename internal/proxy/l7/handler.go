package l7

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// BackendResolver maps (task, groupID) to a backend URL string.
type BackendResolver func(task, groupID string) (string, error)

// Attribution maps (task, groupID) to the lab a request reaches. It fails when
// the group does not own that lab.
type Attribution func(task, groupID string) (lab string, ok bool)

// Authorizer decides whether a LabGroupClient of a group may reach a lab. It is
type Authorizer func(groupID, client, lab string) bool

type Handler struct {
	// keys verifies the tenants' handoff tokens; secret signs and verifies the
	// proxy's own session cookie.
	keys        KeyLookup
	groupTenant GroupTenant
	secret      []byte
	baseDomain  string
	cookieName  string
	resolver    BackendResolver
	transport   http.RoundTripper

	meter     *Meter
	attribute Attribution
	authorize Authorizer
	now       func() time.Time
	// handoffMax bounds exp - iat of a handoff token. The session is sliding: it expires sessionIdle after the last
	// request, is renewed when less than sessionRenew of it remains, and ends at sessionMax at the latest.
	handoffMax, sessionIdle, sessionRenew, sessionMax time.Duration
	// live holds the requests in flight so a lost access can cut them (see live.go); liveMax caps their lifetime.
	live    *liveSet
	liveMax time.Duration
	// caps bound the requests in flight (see LiveCaps); authLimit limits the handoff path.
	caps      LiveCaps
	authLimit *bucketSet
}

// upstreamTransport skips certificate verification for in-cluster backends
// that may use self-signed certificates. The trust boundary is the
// NetworkPolicy that admits only laboratory-proxy/app=proxy.
var upstreamTransport = &http.Transport{
	TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}, // #nosec G402 — see comment above
	MaxIdleConns: 100,
	// Every task of a team is one upstream host: without this the default of 2 idle connections per host
	// makes every request beyond the second concurrent one open (and close) its own TCP connection.
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     90 * time.Second,
}

func NewHandler(keys KeyLookup, secret []byte, baseDomain, cookieName string, resolver BackendResolver) *Handler {
	return &Handler{
		keys:       keys,
		secret:     secret,
		baseDomain: baseDomain,
		cookieName: cookieName,
		resolver:   resolver,
		transport:  upstreamTransport,
		now:        time.Now,
		live:       newLiveSet(),
		handoffMax: DefaultHandoffLifetime, sessionIdle: DefaultSessionIdleTTL, sessionRenew: DefaultSessionRenewBefore, sessionMax: DefaultSessionMaxTTL,
	}
}

// WithLimits sets the longest accepted handoff token (exp - iat) and the sliding session: its idle lifetime, the
// remaining time under which the cookie is renewed, and the absolute maximum.
func (h *Handler) WithLimits(handoffMax, sessionIdle, sessionRenew, sessionMax time.Duration) *Handler {
	h.handoffMax, h.sessionIdle, h.sessionRenew, h.sessionMax = handoffMax, sessionIdle, sessionRenew, sessionMax
	return h
}

// WithAccounting counts requests of a client per lab device. attribute
// maps the task host to the lab and device; without it nothing is counted.
func (h *Handler) WithAccounting(meter *Meter, attribute Attribution) *Handler {
	h.meter, h.attribute = meter, attribute
	return h
}

// WithGroupTenant tells the owner of a group. A handoff whose issuer is not the tenant of its group
// is refused, and so is a session cookie whose tenant no longer owns the group. Without it no
// handoff is accepted.
func (h *Handler) WithGroupTenant(g GroupTenant) *Handler {
	h.groupTenant = g
	return h
}

// WithAuthorizer restricts clients to the labs the group policy allows.
func (h *Handler) WithAuthorizer(authorize Authorizer) *Handler {
	h.authorize = authorize
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	task, err := getTaskName(r, h.baseDomain)
	if err != nil {
		fail(w, r, http.StatusBadRequest, pageGone, "bad request")
		return
	}
	if r.URL.Path == AuthPath {
		if !h.authLimit.allow(peerKey(r)) {
			w.Header().Set("Retry-After", "5")
			fail(w, r, http.StatusTooManyRequests, pageBusy, "too many requests")
			return
		}
		h.handoff(w, r, task)
		return
	}
	claims, err := validateCookie(r, h.secret, h.cookieName, h.now)
	if err != nil {
		h.expired(w, r)
		return
	}

	client := claims.client()
	if client == "" {
		h.expired(w, r)
		return
	}
	if h.groupTenant != nil {
		if owner, ok := h.groupTenant(claims.GroupID); !ok || owner != claims.Tenant {
			fail(w, r, http.StatusNotFound, pageGone, "not found")
			return
		}
	}

	h.renewSession(w, claims)

	backendURL, err := h.resolver(task, claims.GroupID)
	if err != nil {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return
	}
	target, err := url.Parse(backendURL)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, pageFailed, "internal error")
		return
	}
	var lab string
	if h.attribute != nil {
		lab, _ = h.attribute(task, claims.GroupID)
		if lab == "" {
			fail(w, r, http.StatusNotFound, pageGone, "not found")
			return
		}
	}
	if h.authorize != nil && !h.authorize(claims.GroupID, client, lab) {
		fail(w, r, http.StatusNotFound, pageGone, "not found")
		return
	}

	// Strip challenge cookie before forwarding.
	r = r.Clone(r.Context())
	var kept []string
	for _, c := range r.Cookies() {
		if c.Name != h.cookieName {
			kept = append(kept, c.Name+"="+c.Value)
		}
	}
	r.Header.Set("Cookie", strings.Join(kept, "; "))
	r.Header.Set("X-Forwarded-Proto", "https")

	// Track the request: when the client loses access, CheckLive cancels it or closes its upgraded connection.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	entry := &liveEntry{group: claims.GroupID, client: client, lab: lab, tenant: claims.Tenant, deadline: h.liveDeadline(claims.Abs), cancel: cancel}
	if !h.live.tryAdd(entry, h.caps) {
		w.Header().Set("Retry-After", "5")
		fail(w, r, http.StatusTooManyRequests, pageBusy, "too many requests")
		return
	}
	defer h.live.remove(entry)
	w = &hijackRecorder{ResponseWriter: w, entry: entry, cookieName: h.cookieName, host: r.Host, baseDomain: h.baseDomain}

	deviceHost := r.Host
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = h.transport
	// A device must not set (or reset) the proxy's own session cookie: drop any Set-Cookie of that name from its responses.
	proxy.ModifyResponse = func(resp *http.Response) error {
		deviceResponseFilter(resp.Header, h.cookieName, deviceHost, h.baseDomain)
		return nil
	}

	// Count only what a client did to a known lab device, keyed by (group,
	// client, lab); the platform decides which groups are event traffic.
	if h.meter == nil || lab == "" {
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
			fail(w, r, http.StatusBadGateway, pageUpstream, "bad gateway")
		}
		proxy.ServeHTTP(w, r)
		return
	}
	start := h.now()
	out := &countingBody{}
	if r.Body != nil && r.Body != http.NoBody {
		out.ReadCloser = r.Body
		r.Body = out
	}
	rec := &countingWriter{ResponseWriter: w}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		rec.upstreamFailed = true
		fail(w, r, http.StatusBadGateway, pageUpstream, "bad gateway")
	}
	proxy.ServeHTTP(rec, r)
	h.meter.Record(ns(claims.GroupID), client, lab, start, !rec.upstreamFailed, rec.bytes, out.n.Load())
}

func ns(groupID string) string { return GroupNamespace(groupID) }

// GroupNamespace is the namespace of a LabGroup the platform calls groupID: the group's custom resource is named
// after the id (encoded when the id is not a valid name). The namespace is the one in the group's status (a group
// created before the namespace prefix keeps its old one), else the name a new group gets.
func GroupNamespace(groupID string) string {
	groupNamespaceMu.RLock()
	r := groupReader
	groupNamespaceMu.RUnlock()
	name := names.EncodeName(groupID)
	if r != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var g laboratoryv1alpha1.LabGroup
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &g); err == nil {
			return laboratoryv1alpha1.LabGroupNamespaceOf(&g)
		}
	}
	return laboratoryv1alpha1.LabGroupNamespace(name)
}

var (
	groupNamespaceMu sync.RWMutex
	groupReader      client.Reader
)

// UseGroupReader makes GroupNamespace read the LabGroup (from the proxy's cache) to find its namespace.
func UseGroupReader(r client.Reader) {
	groupNamespaceMu.Lock()
	groupReader = r
	groupNamespaceMu.Unlock()
}

// countingWriter records how many body bytes went to the client and whether
// the proxy itself failed to reach the lab. Upgraded (WebSocket) connections
// are hijacked, so their frames are not counted; the request still is.
type countingWriter struct {
	http.ResponseWriter
	bytes          int64
	upstreamFailed bool
}

func (w *countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and Hijack.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// countingBody counts request body bytes read by the upstream transport.
type countingBody struct {
	io.ReadCloser
	n atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.ReadCloser == nil {
		return 0, io.EOF
	}
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error {
	if b.ReadCloser == nil {
		return nil
	}
	return b.ReadCloser.Close()
}

// ServiceResolver builds a BackendResolver that determines backend URL from Service named port.
// getServiceProtocol(task, namespace) returns the Service's first port name ("http" or "https").
func ServiceResolver(getServiceProtocol func(task, namespace string) (string, error)) BackendResolver {
	return func(task, groupID string) (string, error) {
		ns := GroupNamespace(groupID)
		proto, err := getServiceProtocol(task, ns)
		if err != nil {
			return "", err
		}
		port := 80
		if proto == "https" {
			port = 443
		}
		return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", proto, task, ns, port), nil
	}
}

// stripSessionCookie removes the Set-Cookie headers that name the proxy's session cookie, whatever their attributes
// (domain, path, expiry): the cookie is the proxy's, never the device's to set, replace or clear.
func stripSessionCookie(hdr http.Header, name string) {
	filterSetCookies(hdr, func(cookieName, _ string) bool { return cookieName != name })
}

// filterSetCookies keeps the Set-Cookie headers for which keep(name, domain) is true.
func filterSetCookies(hdr http.Header, keep func(name, domain string) bool) {
	values := hdr.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	kept := make([]string, 0, len(values))
	for _, v := range values {
		parts := strings.Split(v, ";")
		cookieName, _, _ := strings.Cut(parts[0], "=")
		domain := ""
		for _, attr := range parts[1:] {
			k, val, _ := strings.Cut(strings.TrimSpace(attr), "=")
			if strings.EqualFold(k, "domain") {
				domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(val), "."))
			}
		}
		if keep(strings.TrimSpace(cookieName), domain) {
			kept = append(kept, v)
		}
	}
	hdr.Del("Set-Cookie")
	for _, v := range kept {
		hdr.Add("Set-Cookie", v)
	}
}

// deviceResponseFilter is what a device's response may not do to the site around it. The labs are served on subdomains of one base domain
// that may itself sit under the platform's, so a device (its author is a participant) must not be able to:
//   - set a cookie for the whole base domain or any parent of it (a cookie that is host-only, or whose Domain is exactly the device's own
//     host, is left as it is);
//   - set a cookie named like the proxy's session cookie, whatever its domain;
//   - clear the site's data (Clear-Site-Data), pin it to HTTPS for good (Strict-Transport-Security) or set its Content-Security-Policy.
func deviceResponseFilter(hdr http.Header, sessionCookie, deviceHost, baseDomain string) {
	deviceHost = strings.ToLower(deviceHost)
	if h, _, ok := strings.Cut(deviceHost, ":"); ok {
		deviceHost = h
	}
	baseDomain = strings.ToLower(baseDomain)
	for _, name := range []string{"Clear-Site-Data", "Strict-Transport-Security", "Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		hdr.Del(name)
	}
	filterSetCookies(hdr, func(name, domain string) bool {
		if name == sessionCookie {
			return false
		}
		if domain == "" || domain == deviceHost {
			return true
		}
		// the base domain or a parent of it
		return domain != baseDomain && !strings.HasSuffix(baseDomain, "."+domain)
	})
}
