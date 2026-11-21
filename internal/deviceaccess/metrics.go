package deviceaccess

import "github.com/prometheus/client_golang/prometheus"

type metrics struct {
	active   *prometheus.GaugeVec
	sessions *prometheus.CounterVec
	bytes    *prometheus.CounterVec
	refused  *prometheus.CounterVec
}

// newMetrics registers the metrics. None of them is labelled with a lab, a device or a client: those are unbounded, and the
// per-session log line carries them.
func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		active:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "device_access_streams_active", Help: "Open streams by kind."}, []string{"kind"}),
		sessions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "device_access_streams_total", Help: "Finished streams by kind and close reason."}, []string{"kind", "reason"}),
		bytes:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "device_access_bytes_total", Help: "Bytes moved, by kind and direction (in: towards the device)."}, []string{"kind", "direction"}),
		refused:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "device_access_refused_total", Help: "Refused open requests by code."}, []string{"code"}),
	}
	reg.MustRegister(m.active, m.sessions, m.bytes, m.refused)
	return m
}
