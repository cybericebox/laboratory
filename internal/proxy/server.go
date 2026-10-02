package proxy

import (
	"crypto/tls"
	"net/http"
)

// NewHTTPServer is the L7 proxy's HTTPS server with the limits that apply before any routing or authentication: a client
// that dribbles its headers or its body, or opens connections and says nothing, is cut instead of holding a connection
// and a goroutine forever. There is no write timeout (responses stream; an upgraded connection is bounded by the live
// check), and a hijacked connection has its deadlines cleared by the handler.
func NewHTTPServer(cfg *L7Config, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
}
