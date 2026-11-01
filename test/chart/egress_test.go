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

// The connection log of the egress is on a volume of the operator by default: a claim, an operator that is replaced in place (Recreate),
// the directory in its settings; turned off, nothing of it is left.
func TestEgressLogVolume(t *testing.T) {
	render := func(extra ...string) string {
		t.Helper()
		out, err := helmTemplate(t, append(append([]string{}, extra...), "-s", "templates/operator/deployment.yaml")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	claim := func(extra ...string) string {
		t.Helper()
		out, err := helmTemplate(t, append(append([]string{}, extra...), "-s", "templates/operator/egress-log-pvc.yaml")...)
		if err != nil {
			return "" // a template that renders nothing is not found by helm
		}
		return out
	}
	on := claim() + render()
	for _, want := range []string{"kind: PersistentVolumeClaim", "name: laboratory-egress-log", "storage: \"10Gi\"", "type: Recreate", "mountPath: /var/lib/egress-log", "claimName: laboratory-egress-log", "fsGroup: 65532"} {
		if !strings.Contains(on, want) {
			t.Errorf("default: missing %q", want)
		}
	}
	if operatorConfig(t)["EGRESS_LOG_DIR"] != "/var/lib/egress-log" {
		t.Errorf("the operator must be told the directory: %q", operatorConfig(t)["EGRESS_LOG_DIR"])
	}
	sized := claim("--set", "internetEgress.log.persistence.size=50Gi", "--set", "internetEgress.log.persistence.storageClass=fast")
	if !strings.Contains(sized, `storage: "50Gi"`) || !strings.Contains(sized, `storageClassName: "fast"`) {
		t.Errorf("size and class:\n%s", sized)
	}
	for _, off := range [][]string{{"--set", "internetEgress.log.persistence.enabled=false"}, {"--set", "internetEgress.monitor.enabled=false"}, {"--set", "internetEgress.log.dir=/mnt/log"}} {
		out := render(off...) + claim(off...)
		for _, not := range []string{"PersistentVolumeClaim", "type: Recreate", "fsGroup"} {
			if strings.Contains(out, not) {
				t.Errorf("%v: %q must not be rendered", off, not)
			}
		}
	}
	if operatorConfig(t, "--set", "internetEgress.log.persistence.enabled=false")["EGRESS_LOG_DIR"] != "" {
		t.Error("without persistence and a directory the operator keeps no files")
	}
	if operatorConfig(t, "--set", "internetEgress.log.dir=/mnt/log")["EGRESS_LOG_DIR"] != "/mnt/log" {
		t.Error("a directory of your own is passed as it is")
	}
}

func TestEgressHostRatesReachTheOperator(t *testing.T) {
	cfg := operatorConfig(t, "--set", "internetEgress.proxy.hostNewPerSecond=7", "--set", "internetEgress.proxy.hostGatewayNewPerSecond=30")
	if cfg["EGRESS_HOST_NEW_PER_SECOND"] != "7" || cfg["EGRESS_HOST_GATEWAY_NEW_PER_SECOND"] != "30" {
		t.Errorf("rates: %q %q", cfg["EGRESS_HOST_NEW_PER_SECOND"], cfg["EGRESS_HOST_GATEWAY_NEW_PER_SECOND"])
	}
}
