package laboratory

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func labWithVPN(enabled, reverse bool) *laboratoryv1alpha1.Lab {
	l := &laboratoryv1alpha1.Lab{}
	l.Spec.VPN.Enabled, l.Spec.VPN.ReverseConnections = enabled, reverse
	return l
}

func legWith(blocked bool, dropped int64) *laboratoryv1alpha1.LabVPN {
	leg := &laboratoryv1alpha1.LabVPN{}
	leg.Status.LabInitiatedBlocked, leg.Status.LabInitiatedDropped = blocked, dropped
	return leg
}

// A lab is reported as blocked only when the gateway confirmed it: an old VPN pod never does.
func TestLabIsReportedBlockedOnlyWhenTheGatewayConfirmed(t *testing.T) {
	for name, tc := range map[string]struct {
		lab  *laboratoryv1alpha1.Lab
		leg  *laboratoryv1alpha1.LabVPN
		want bool
	}{
		"confirmed":                            {labWithVPN(true, false), legWith(true, 0), true},
		"old gateway: never confirms":          {labWithVPN(true, false), legWith(false, 0), false},
		"no leg yet":                           {labWithVPN(true, false), nil, false},
		"switch on":                            {labWithVPN(true, true), legWith(true, 0), false},
		"no vpn":                               {labWithVPN(false, false), legWith(true, 0), false},
		"stale confirmation, switch turned on": {labWithVPN(true, true), legWith(true, 5), false},
	} {
		if got := labInitiatedBlockedStatus(tc.lab, tc.leg); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestDroppedPacketsMetricFollowsTheLeg(t *testing.T) {
	vpnLabInitiatedDropped.Reset()
	publishLabInitiatedDropped("g", "a", legWith(true, 7), true)
	if n := seriesCount(); n != 1 {
		t.Fatalf("series %d", n)
	}
	if v := seriesValue(t); v != 7 {
		t.Fatalf("value %v", v)
	}
	publishLabInitiatedDropped("g", "a", legWith(false, 7), true) // an old gateway: nothing to report
	if n := seriesCount(); n != 0 {
		t.Fatalf("series %d after the confirmation went away", n)
	}
	publishLabInitiatedDropped("g", "a", legWith(true, 3), true)
	publishLabInitiatedDropped("g", "a", nil, false) // the lab is gone
	if n := seriesCount(); n != 0 {
		t.Fatalf("series %d for a deleted lab", n)
	}
}

func seriesCount() int {
	ch := make(chan prometheus.Metric, 16)
	vpnLabInitiatedDropped.Collect(ch)
	close(ch)
	return len(ch)
}

func seriesValue(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := vpnLabInitiatedDropped.WithLabelValues("g", "a").Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}
