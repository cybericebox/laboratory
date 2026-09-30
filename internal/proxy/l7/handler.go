package l7

import (
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// BackendResolver maps (task, groupID) to a backend URL string.
type BackendResolver func(task, groupID string) (string, error)

// Attribution maps (task, groupID) to the lab a request reaches. It fails when
// the group does not own that lab.
type Attribution func(task, groupID string) (lab string, ok bool)

// Authorizer decides whether a LabGroupClient of a group may reach a lab. It is
// called only for tokens that name a client.
type Authorizer func(groupID, client, lab string) bool

type Handler struct {
	key        func() *rsa.PublicKey
	baseDomain string
	cookieName string
	resolver   BackendResolver
	transport  http.RoundTripper

	mode      TokenMode
	meter     *Meter
	attribute Attribution
	authorize Authorizer
	now       func() time.Time
}

// upstreamTransport skips certificate verification for in-cluster backends
// that may use self-signed certificates. The trust boundary is the
// NetworkPolicy that admits only laboratory-proxy/app=proxy.
var upstreamTransport = &http.Transport{
	TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}, // #nosec G402 — see comment above
	MaxIdleConns:    100,
	IdleConnTimeout: 90 * time.Second,
}

func NewHandler(key func() *rsa.PublicKey, baseDomain, cookieName string, resolver BackendResolver) *Handler {
	return &Handler{
		key:        key,
		baseDomain: baseDomain,
		cookieName: cookieName,
		resolver:   resolver,
		transport:  upstreamTransport,
		mode:       ModeLegacy,
		now:        time.Now,
	}
}

// WithTokenMode sets how strictly tokens must name a user.
func (h *Handler) WithTokenMode(mode TokenMode) *Handler {
	h.mode = mode
	return h
}

// WithAccounting counts requests of per-user tokens per lab device. attribute
// maps the task host to the lab and device; without it nothing is counted.
func (h *Handler) WithAccounting(meter *Meter, attribute Attribution) *Handler {
	h.meter, h.attribute = meter, attribute
	return h
}

// WithAuthorizer restricts per-user tokens to the labs the group policy allows.
func (h *Handler) WithAuthorizer(authorize Authorizer) *Handler {
	h.authorize = authorize
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	task, err := getTaskName(r, h.baseDomain)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	claims, err := validateCookie(r, h.key, h.cookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	client := claims.client()
	if h.mode == ModePerUser && client == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.mode == ModeLegacy {
		client = ""
	}

	backendURL, err := h.resolver(task, claims.GroupID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	target, err := url.Parse(backendURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var lab string
	if h.attribute != nil {
		lab, _ = h.attribute(task, claims.GroupID)
		if lab == "" && h.mode == ModePerUser {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	if client != "" && h.authorize != nil && !h.authorize(claims.GroupID, client, lab) {
		http.Error(w, "forbidden", http.StatusForbidden)
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

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = h.transport

	// Count only what a client did to a known lab device, keyed by (group,
	// client, lab); the platform decides which groups are event traffic. A
	// request without a client cannot be attributed to anybody.
	if h.meter == nil {
		proxy.ServeHTTP(w, r)
		return
	}
	if client == "" || lab == "" {
		h.meter.RecordLegacy()
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
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		rec.upstreamFailed = true
		w.WriteHeader(http.StatusBadGateway)
	}
	proxy.ServeHTTP(rec, r)
	h.meter.Record(ns(claims.GroupID), client, lab, start, !rec.upstreamFailed, rec.bytes, out.n.Load())
}

func ns(groupID string) string { return laboratoryv1alpha1.LabGroupNamespace(groupID) }

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
		ns := laboratoryv1alpha1.LabGroupNamespace(groupID)
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
