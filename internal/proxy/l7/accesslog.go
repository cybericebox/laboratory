package l7

import (
	"context"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// DefaultSlowRequest is the upstream latency above which a request is flagged slow in the access log.
const DefaultSlowRequest = 2 * time.Second

// maxLoggedPath bounds the path in a log line: a client chooses it.
const maxLoggedPath = 200

// accessLog writes one structured line per proxied request: the lab, the method, the path without the query, the status, the
// upstream latency (until the lab answered with its headers), the bytes sent back and whether the client left before the end.
// No address of a client is ever logged. A request is slow when the upstream took longer than slow.
type accessLog struct {
	log  logr.Logger
	slow time.Duration
}

// WithAccessLog turns on the access log; slow <= 0 means DefaultSlowRequest.
func (h *Handler) WithAccessLog(log logr.Logger, slow time.Duration) *Handler {
	if slow <= 0 {
		slow = DefaultSlowRequest
	}
	h.accessLog = &accessLog{log: log, slow: slow}
	return h
}

// accessRecorder sees what a request gets: the final status, the body bytes and the upstream latency.
type accessRecorder struct {
	http.ResponseWriter
	status   int
	bytes    int64
	upstream time.Duration
}

func (w *accessRecorder) WriteHeader(code int) {
	// An informational (1xx) response is not the answer, except the switch to another protocol.
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and Hijack.
func (w *accessRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// timedTransport measures how long the upstream takes to answer with its headers (or to fail).
type timedTransport struct {
	base http.RoundTripper
	rec  *accessRecorder
}

func (t *timedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(r)
	t.rec.upstream = time.Since(start)
	return resp, err
}

// emit writes the line of a finished request. clientCtx is the context of the connection's request, canceled when the client left.
func (a *accessLog) emit(rec *accessRecorder, r *http.Request, lab string, clientCtx context.Context) {
	path := r.URL.Path
	if len(path) > maxLoggedPath {
		path = path[:maxLoggedPath]
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	a.log.Info("request",
		"lab", lab,
		"method", r.Method,
		"path", path,
		"status", status,
		"upstreamMs", rec.upstream.Milliseconds(),
		"bytes", rec.bytes,
		"clientCancel", clientCtx.Err() != nil,
		"slow", rec.upstream > a.slow,
	)
}
