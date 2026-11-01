package chart_test

import (
	"strings"
	"testing"
)

// The internet egress settings reach the operator, and the operator's network policy can pull the summaries of the gateways.
func TestInternetEgressSettingsReachTheOperator(t *testing.T) {
	cfg := operatorConfig(t)
	for k, want := range map[string]string{
		"EGRESS_MONITOR_ENABLED": "true", "EGRESS_STATS_PORT": "9188", "EGRESS_LOG_RETENTION": "720h", "EGRESS_SIGNAL_HOST_LABS": "20",
		"EGRESS_SIGNAL_CAP_MINUTES": "3", "EGRESS_DNS_UPSTREAMS": "1.1.1.1,9.9.9.9", "BANDWIDTH_LAB_UPLOAD_KBPS": "2048", "BANDWIDTH_UPLOAD_TOTAL_MBIT": "100",
	} {
		if cfg[k] != want {
			t.Errorf("%s = %q, want %q", k, cfg[k], want)
		}
	}
	custom := operatorConfig(t, "--set", "internetEgress.log.dir=/var/egress", "--set", "internetEgress.log.retentionDays=7", "--set", "internetEgress.proxy.tunnelSuffixes=-",
		"--set", "internetEgress.statsPort=9300", "--set", "bandwidth.caps.labUploadKbps=1024")
	for k, want := range map[string]string{"EGRESS_LOG_DIR": "/var/egress", "EGRESS_LOG_RETENTION": "168h", "EGRESS_TUNNEL_SUFFIXES": "-", "EGRESS_STATS_PORT": "9300", "BANDWIDTH_LAB_UPLOAD_KBPS": "1024"} {
		if custom[k] != want {
			t.Errorf("%s = %q, want %q", k, custom[k], want)
		}
	}
	out, err := helperTemplateNetpol(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "laboratory.cybericebox.com/component: gateway") || !strings.Contains(out, `port: "9188"`) {
		t.Errorf("the operator's policy does not let it reach the summaries of the gateways:\n%s", out)
	}
}

func helperTemplateNetpol(t *testing.T) (string, error) {
	t.Helper()
	return helmTemplate(t, "--set", "operator.networkPolicy.enabled=true", "-s", "templates/netpol/controller.yaml")
}
