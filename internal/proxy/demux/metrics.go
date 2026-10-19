package demux

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// FastPathResults are the counters of the in-kernel fast path, in the order of its enum wg_stat.
var FastPathResults = []string{
	"forwarded_to_server", "forwarded_to_client", "passed_miss", "passed_source", "passed_stale", "dropped_rate",
	"passed_route", "passed_meta",
}

var messageTypes = [5]string{1: "handshake_initiation", 2: "handshake_response", 3: "cookie_reply", 4: "transport"}

var (
	modeDesc       = prometheus.NewDesc("wg_demux_mode", "The mode the demux moves transport packets in (1 for the active one).", []string{"mode"}, nil)
	sessionsDesc   = prometheus.NewDesc("wg_demux_sessions", "Completed live sessions.", nil, nil)
	sinkErrorsDesc = prometheus.NewDesc("wg_demux_sink_errors_total", "Sessions (directions) that could not be written into the fast path; the slow path keeps forwarding them.", nil, nil)
	packetsDesc    = prometheus.NewDesc("wg_demux_packets_total", "Datagrams the demux socket took, by WireGuard message type.", []string{"type"}, nil)
	fastDesc       = prometheus.NewDesc("wg_demux_fastpath_packets_total", "Transport packets the in-kernel fast path judged, by result (summed over the CPUs).", []string{"result"}, nil)
)

// collector reads everything at scrape time: the demux keeps its own counters, so nothing is updated per packet here.
type collector struct {
	mode  Mode
	demux *Demux
	fast  func() map[string]uint64
}

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{modeDesc, sessionsDesc, sinkErrorsDesc, packetsDesc, fastDesc} {
		ch <- d
	}
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(modeDesc, prometheus.GaugeValue, 1, string(c.mode))
	ch <- prometheus.MustNewConstMetric(sessionsDesc, prometheus.GaugeValue, float64(c.demux.conntrack.SessionCount()))
	ch <- prometheus.MustNewConstMetric(sinkErrorsDesc, prometheus.CounterValue, float64(c.demux.conntrack.SinkErrors()))
	received := c.demux.Received()
	for typ := 1; typ < len(messageTypes); typ++ {
		ch <- prometheus.MustNewConstMetric(packetsDesc, prometheus.CounterValue, float64(received[typ]), messageTypes[typ])
	}
	if c.fast == nil {
		return
	}
	counts := c.fast()
	for _, r := range FastPathResults {
		ch <- prometheus.MustNewConstMetric(fastDesc, prometheus.CounterValue, float64(counts[r]), r)
	}
}

// MetricsHandler serves the demux's metrics in the Prometheus format. fast reads the fast path's counters by result (nil in
// userspace mode).
func MetricsHandler(mode Mode, d *Demux, fast func() map[string]uint64) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector{mode: mode, demux: d, fast: fast})
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}
