package registrygate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Gate is the HTTP handler in front of zot: it refuses what checkPath refuses and passes everything else to zot unchanged, streaming,
// with the client's Authorization (zot's own accounts decide on the pass-through paths).
type Gate struct {
	proxy *httputil.ReverseProxy
	zot   string
	// Metrics is served on the health port, when set.
	Metrics http.Handler
	// Cache is the cache path (/v2/c/...). Without it that path is refused: zot is never reached by it.
	Cache http.Handler
	// CacheTLSOnly refuses the cache path on a connection that is not TLS: the pull's credentials are in it.
	CacheTLSOnly bool
}

// New makes the gate for a zot at addr (host:port, plain HTTP, on the loopback of the pod).
func New(zotAddr string) (*Gate, error) {
	target, err := url.Parse("http://" + zotAddr)
	if err != nil || target.Host == "" {
		return nil, errors.New("registrygate: zot address must be host:port")
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// zot builds the Location of an upload from the Host header, and the client must get its own registry name back
			// (localhost:5035 on a node): SetURL rewrites Host to the target's, so it is put back.
			r.SetURL(target)
			r.Out.Host = r.In.Host
		},
		FlushInterval: -1, // blobs are streamed, never buffered
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the registry store is not reachable", http.StatusBadGateway)
		},
	}
	return &Gate{proxy: p, zot: target.Host}, nil
}

// ServeHTTP implements http.Handler.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := checkPath(r.URL.EscapedPath()); err != nil {
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}
	if isCache(r) {
		if g.Cache == nil {
			refuse(w, http.StatusNotFound, "NAME_UNKNOWN", "the image cache is off", false)
			return
		}
		if g.CacheTLSOnly && r.TLS == nil {
			refuse(w, http.StatusForbidden, "DENIED", "the image cache is served over TLS only", false)
			return
		}
		g.Cache.ServeHTTP(w, r)
		return
	}
	g.proxy.ServeHTTP(w, r)
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
	}
}
