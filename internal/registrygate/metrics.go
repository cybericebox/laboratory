package registrygate

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are the counters of the gate. Labels carry no credential, no tenant and no hash.
type Metrics struct {
	reg         *prometheus.Registry
	refusals    *prometheus.CounterVec
	decisions   *prometheus.CounterVec
	checks      *prometheus.CounterVec
	cache       *prometheus.CounterVec
	fills       *prometheus.CounterVec
	filledBytes prometheus.Counter
	lab         *prometheus.CounterVec
	auth        *prometheus.CounterVec
}

// NewMetrics makes the counters on a registry of their own.
func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	vec := func(name, help string, labels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_gate_" + name, Help: help}, labels)
		m.reg.MustRegister(v)
		return v
	}
	m.refusals = vec("cache_refusals_total", "Cache-path requests refused before any check.", "reason")
	m.decisions = vec("decisions_total", "What the decision table said (serve, stale, check, refused).", "action")
	m.checks = vec("upstream_checks_total", "Fresh checks against an upstream registry by outcome.", "outcome")
	m.cache = vec("cache_total", "Objects served by the cache (hit, miss, mounted).", "kind", "result")
	m.fills = vec("fills_total", "Fills of the cache from upstream by result.", "kind", "result")
	m.lab = vec("lab_requests_total", "Requests on the snapshot repositories by what the gate did (allowed, refused by code).", "result")
	m.auth = vec("authentications_total", "Authentications of callers by result (ok, unauthenticated, forbidden, unavailable) and kind.", "result")
	m.filledBytes = prometheus.NewCounter(prometheus.CounterOpts{Name: "registry_gate_filled_bytes_total", Help: "Bytes of blobs stored from upstream."})
	m.reg.MustRegister(m.filledBytes)
	return m
}

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler { return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}) }
