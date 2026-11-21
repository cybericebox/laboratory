package laboratory

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func gatewayWith(kind laboratoryv1alpha1.GatewayKind, reverse bool) laboratoryv1alpha1.GatewayTemplate {
	return laboratoryv1alpha1.GatewayTemplate{Name: string(kind), Kind: kind, ReverseConnections: reverse}
}

func legWith(blocked bool, dropped int64) *laboratoryv1alpha1.LabVPN {
	leg := &laboratoryv1alpha1.LabVPN{}
	leg.Status.LabInitiatedBlocked, leg.Status.LabInitiatedDropped = blocked, dropped
	return leg
}

// A lab is reported as blocked only when the gateway confirmed it: an old VPN pod never does.
func TestLabIsReportedBlockedOnlyWhenTheGatewayConfirmed(t *testing.T) {
	for name, tc := range map[string]struct {
		gateway laboratoryv1alpha1.GatewayTemplate
		leg     *laboratoryv1alpha1.LabVPN
		want    bool
	}{
		"confirmed":                            {gatewayWith(laboratoryv1alpha1.GatewayKindVPN, false), legWith(true, 0), true},
		"old gateway: never confirms":          {gatewayWith(laboratoryv1alpha1.GatewayKindVPN, false), legWith(false, 0), false},
		"no leg yet":                           {gatewayWith(laboratoryv1alpha1.GatewayKindVPN, false), nil, false},
		"switch on":                            {gatewayWith(laboratoryv1alpha1.GatewayKindVPN, true), legWith(true, 0), false},
		"internet gateway":                     {gatewayWith(laboratoryv1alpha1.GatewayKindInternet, false), legWith(true, 0), false},
		"stale confirmation, switch turned on": {gatewayWith(laboratoryv1alpha1.GatewayKindVPN, true), legWith(true, 5), false},
	} {
		if got := labInitiatedBlockedStatus(tc.gateway, tc.leg); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestDroppedPacketsMetricFollowsTheLeg(t *testing.T) {
	vpnLabInitiatedDropped.Reset()
	publishLabInitiatedDropped("g", "a", 7, true)
	if n := seriesCount(); n != 1 {
		t.Fatalf("series %d", n)
	}
	if v := seriesValue(t); v != 7 {
		t.Fatalf("value %v", v)
	}
	publishLabInitiatedDropped("g", "a", 7, false) // an old gateway: nothing to report
	if n := seriesCount(); n != 0 {
		t.Fatalf("series %d after the confirmation went away", n)
	}
	publishLabInitiatedDropped("g", "a", 3, true)
	publishLabInitiatedDropped("g", "a", 0, false) // the lab is gone
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
