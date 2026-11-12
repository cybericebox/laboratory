package deviceaccess

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// StreamPath (wire.StreamPath) is where the proxy opens a stream, a WebSocket. HealthPath answers 200 while the service runs.
const (
	StreamPath = wire.StreamPath
	HealthPath = "/healthz"
)

// wsConn adapts a WebSocket to Conn.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) (bool, []byte, error) {
	typ, msg, err := w.c.Read(ctx)
	return typ == websocket.MessageText, msg, err
}
func (w wsConn) WriteText(ctx context.Context, msg []byte) error {
	return w.c.Write(ctx, websocket.MessageText, msg)
}
func (w wsConn) WriteBinary(ctx context.Context, msg []byte) error {
	return w.c.Write(ctx, websocket.MessageBinary, msg)
}

// Handler is the listener of the service: the stream endpoint and the health endpoint. It is plain HTTP on the pod network; the
// Cilium policy of the chart lets only the L7 proxy pods connect.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc(StreamPath, func(w http.ResponseWriter, r *http.Request) {
		// Compression is off: it would let a stream cost more memory and CPU than its bytes, and a terminal gains little.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		c.SetReadLimit(wire.MaxFrame)
		defer func() { _ = c.CloseNow() }()
		s.Serve(r.Context(), wsConn{c})
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	return mux
}

// NewHTTPServer returns the HTTP server for Handler with the timeouts a listener open to the pod network needs. An upgraded
// connection leaves these timeouts behind; the service bounds a stream with its own deadlines.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10, IdleTimeout: 60 * time.Second}
}

// MetricsHandler serves the metrics of the service.
func (s *Service) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{})
}
