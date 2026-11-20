package registrygate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gate is the HTTP handler in front of zot. Every request is authenticated (the projected ServiceAccount token of the caller, verified
// with the API server) before anything else, and then routed by who the caller is and what it asks for: the image cache (/v2/c/...), the
// snapshot repositories (/v2/lab/...) and the few things the operator does. Nothing the caller sends reaches zot unchecked, no X-Cib-*
// header and no Authorization header is ever passed on, and zot never sees any account but the gate's own.
type Gate struct {
	zot string
	// Metrics is served on the health port, when set.
	Metrics http.Handler
	M       *Metrics
	// Auth turns a request into a principal.
	Auth *Authenticator
	// Cache is the cache path (/v2/c/...). Without it that path is refused: zot is never reached by it.
	Cache *Cache
	// Lab is the snapshot path (/v2/lab/..., /v2/_catalog, /v2/base/...). Without it that path is refused.
	Lab *LabGate
	// TLSOnly refuses every request on a connection that is not TLS: the token is in it.
	TLSOnly bool
}

// New makes the gate for a zot at addr (host:port, plain HTTP, on the loopback of the pod).
func New(zotAddr string) (*Gate, error) {
	target, err := url.Parse("http://" + zotAddr)
	if err != nil || target.Host == "" {
		return nil, errors.New("registrygate: zot address must be host:port")
	}
	return &Gate{zot: target.Host}, nil
}

func (g *Gate) authMetric(result string) {
	if g.M != nil {
		g.M.auth.WithLabelValues(result).Inc()
	}
}

// ServeHTTP implements http.Handler.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := checkPath(r.URL.EscapedPath()); err != nil {
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}
	if g.TLSOnly && r.TLS == nil {
		refuse(w, http.StatusForbidden, "DENIED", "the registry is served over TLS only", false)
		return
	}
	if g.Auth == nil {
		refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the registry gate has no authentication", false)
		return
	}
	p, err := g.Auth.Authenticate(r.Context(), r)
	// whatever the outcome, no X-Cib-* header (the token one included) and no Authorization of a node goes any further
	StripPlatformHeaders(r.Header)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		g.authMetric("unauthenticated")
		refuse(w, http.StatusUnauthorized, "UNAUTHORIZED", "a valid node token is required", false)
		return
	case errors.Is(err, ErrRateLimited):
		g.authMetric("rate_limited")
		w.Header().Set("Retry-After", "5")
		refuse(w, http.StatusTooManyRequests, "TOOMANYREQUESTS", "too many token checks from this address", false)
		return
	case errors.Is(err, ErrForbidden):
		g.authMetric("forbidden")
		refuse(w, http.StatusForbidden, "DENIED", "the caller is not accepted", false)
		return
	case err != nil:
		g.authMetric("unavailable")
		w.Header().Set("Retry-After", "30")
		refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the cluster view is not available, try again", false)
		return
	}
	g.authMetric("ok_" + p.Kind.String())
	escaped := r.URL.EscapedPath()
	switch {
	case escaped == "/v2" || escaped == "/v2/":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			refuse(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "only GET", false)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		_, _ = w.Write([]byte("{}"))
	case isCache(r):
		if g.Cache == nil {
			refuse(w, http.StatusNotFound, "NAME_UNKNOWN", "the image cache is off", false)
			return
		}
		g.Cache.Serve(w, r, p)
	case strings.HasPrefix(escaped, "/v2/lab/") || escaped == "/v2/_catalog" || strings.HasPrefix(escaped, "/v2/base/"):
		if g.Lab == nil {
			refuse(w, http.StatusNotFound, "NAME_UNKNOWN", "snapshots are off", false)
			return
		}
		g.Lab.Serve(w, r, p)
	default:
		refuse(w, http.StatusForbidden, "DENIED", "not allowed", false)
	}
}

// Ready says whether zot accepts a connection: the pod is ready only then (the Service routes to the gate).
func (g *Gate) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", g.zot)
	if err != nil {
		return err
	}
	return c.Close()
}

// zotAlive asks zot for its API root over the loopback: any answer below 500 (401 is zot asking for credentials) shows it serves.
func (g *Gate) zotAlive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+g.zot+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("zot answers %d", resp.StatusCode)
	}
	return nil
}

// HealthHandler serves /healthz (the gate answers and zot answers on /v2/), /readyz (zot accepts a connection) and, when there are
// metrics, /metrics, on a port of its own.
func (g *Gate) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := g.zotAlive(r.Context()); err != nil {
			http.Error(w, "the registry store does not answer", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	if g.Metrics != nil {
		mux.Handle("/metrics", g.Metrics)
	}
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := g.Ready(r.Context()); err != nil {
			http.Error(w, "the registry store is not reachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// NewServer is the public listener: a bounded header, no write timeout (a blob is large and slow), an idle timeout.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// the listener gives a new connection a deadline for the TLS handshake and the first request line; it is lifted once a request is
		// being served, so that long uploads and pulls are not cut
		ConnState: func(c net.Conn, st http.ConnState) {
			if st == http.StateActive {
				_ = c.SetReadDeadline(time.Time{})
			}
		},
	}
}

// NewDeadlineListener listens on addr and gives every accepted connection d to finish its TLS handshake and send its first request.
func NewDeadlineListener(addr string, d time.Duration) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &deadlineListener{Listener: l, d: d}, nil
}

type deadlineListener struct {
	net.Listener
	d time.Duration
}

func (l *deadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(l.d))
	return c, nil
}
