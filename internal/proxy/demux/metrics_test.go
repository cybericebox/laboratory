package demux

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsExposeTheDemux(t *testing.T) {
	r := newRig(t, DefaultLimits())
	s := &lockedSink{}
	r.demux.conntrack.SetSink(s, fakeSource{ok: false})
	r.handshake(t, 0x11, 0x22)
	fast := func() map[string]uint64 { return map[string]uint64{"forwarded_to_server": 7, "passed_meta": 2} }
	srv := httptest.NewServer(MetricsHandler(ModeTC, r.demux, fast))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`wg_demux_mode{mode="tc"} 1`,
		`wg_demux_sessions 1`,
		`wg_demux_sink_errors_total 2`,
		`wg_demux_packets_total{type="handshake_initiation"} 1`,
		`wg_demux_packets_total{type="handshake_response"} 1`,
		`wg_demux_packets_total{type="transport"} 0`,
		`wg_demux_fastpath_packets_total{result="forwarded_to_server"} 7`,
		`wg_demux_fastpath_packets_total{result="passed_meta"} 2`,
	} {
		if !strings.Contains(string(body), want+"\n") {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
}

func TestMetricsWithoutFastPath(t *testing.T) {
	r := newRig(t, DefaultLimits())
	srv := httptest.NewServer(MetricsHandler(ModeUserspace, r.demux, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `wg_demux_mode{mode="userspace"} 1`) || strings.Contains(string(body), "wg_demux_fastpath") {
		t.Fatalf("userspace metrics:\n%s", body)
	}
}
