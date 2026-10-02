package chart_test

import (
	"regexp"
	"strings"
	"testing"
)

// The proxy runs two replicas by default, never two on one node (required anti-affinity), with a budget that keeps
// one up during a drain, and both containers are Guaranteed with the measured limits.
func TestProxyRunsTwoReplicasWithABudgetAndMeasuredLimits(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/deployment.yaml", "-s", "templates/proxy/pdb.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"replicas: 2",
		"kind: PodDisruptionBudget",
		"minAvailable: 1",
		"requiredDuringSchedulingIgnoredDuringExecution",
		"topologyKey: kubernetes.io/hostname",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// requests = limits for l7 (500m/64Mi) and wg-demux (250m/64Mi)
	for _, want := range []string{"cpu: 500m", "cpu: 250m", "memory: 64Mi"} {
		if strings.Count(out, want) < 2 {
			t.Errorf("%q should appear as a request and a limit", want)
		}
	}
}

// A budget on a single replica would block every drain: it is rendered only with more than one replica.
func TestProxyBudgetNeedsMoreThanOneReplica(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/pdb.yaml", "--set", "proxy.replicas=1")
	if err == nil && strings.Contains(out, "PodDisruptionBudget") {
		t.Fatalf("one replica must not get a budget:\n%s", out)
	}
	out, err = helmTemplate(t, "-s", "templates/proxy/pdb.yaml", "--set", "proxy.podDisruptionBudget.enabled=false")
	if err == nil && strings.Contains(out, "PodDisruptionBudget") {
		t.Fatalf("a disabled budget must not render:\n%s", out)
	}
}

// daemonset mode: one pod per node (narrowed by nodeSelector), same pod as the Deployment's, no replicas, no budget.
func TestProxyDaemonSetMode(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.mode=daemonset", "--set-string", "proxy.nodeSelector.lab=true", "--set", "proxy.tolerations[0].operator=Exists",
		"-s", "templates/proxy/daemonset.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"kind: DaemonSet", "name: laboratory-proxy", "app: laboratory-proxy-l7", "nodeSelector:", "tolerations:", "operator: Exists", "lab: \"true\"", "name: l7", "name: wg-demux"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "podAntiAffinity") {
		t.Errorf("a DaemonSet needs no anti-affinity")
	}
	for _, tpl := range []string{"templates/proxy/deployment.yaml", "templates/proxy/pdb.yaml"} {
		if o, err := helmTemplate(t, "--set", "proxy.mode=daemonset", "-s", tpl); err == nil && strings.Contains(o, "kind:") {
			t.Errorf("%s must not render in daemonset mode:\n%s", tpl, o)
		}
	}
}

func TestProxyDeploymentModeHasNoDaemonSet(t *testing.T) {
	if o, err := helmTemplate(t, "-s", "templates/proxy/daemonset.yaml"); err == nil && strings.Contains(o, "kind:") {
		t.Fatalf("deployment mode renders no DaemonSet:\n%s", o)
	}
}

func TestProxyModeIsValidated(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.mode=deployment", "-s", "templates/proxy/deployment.yaml")
	if err == nil || !strings.Contains(out, "proxy.mode") {
		t.Fatalf("an unknown mode must be refused: %v\n%s", err, out)
	}
}

// The proxy's sliding session comes from the chart.
func TestProxySessionValuesReachTheProxy(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.l7.sessionIdleTTL=12h", "--set", "proxy.l7.sessionRenewBefore=30m", "--set", "proxy.l7.sessionMaxTTL=48h",
		"-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for name, val := range map[string]string{"SESSION_IDLE_TTL": "12h", "SESSION_RENEW_BEFORE": "30m", "SESSION_MAX_TTL": "48h"} {
		if !regexp.MustCompile(`name: ` + name + `\s+value: "` + val + `"`).MatchString(out) {
			t.Errorf("%s = %s missing:\n%s", name, val, out)
		}
	}
}

// With required anti-affinity a surge pod cannot be scheduled next to the old one, so the Deployment replaces
// pods one at a time (found when a one-node stand hung in the upgrade).
func TestProxyUpdateDoesNotSurge(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"maxSurge: 0", "maxUnavailable: 1", "requiredDuringSchedulingIgnoredDuringExecution"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestProxyServerLimitsAreValues(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for name, val := range map[string]string{"READ_HEADER_TIMEOUT": "10s", "READ_TIMEOUT": "5m", "IDLE_TIMEOUT": "2m", "MAX_HEADER_BYTES": "65536", "ACCESS_TOKEN_MAX_TTL": "60s"} {
		if !regexp.MustCompile(`name: ` + name + `\s+value: "` + val + `"`).MatchString(out) {
			t.Errorf("%s = %s missing", name, val)
		}
	}
}

func TestDemuxLimitsAreValues(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/deployment.yaml", "--set", "proxy.wg.limits.partialTTL=7s", "--set", "proxy.wg.limits.roamInterval=9s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for name, val := range map[string]string{"DEMUX_MAX_ENTRIES": "100000", "DEMUX_PARTIAL_TTL": "7s",
		"DEMUX_MISS_RATE": "2000", "DEMUX_MISS_BURST": "4000", "DEMUX_ROAM_INTERVAL": "9s", "DEMUX_GLOBAL_HANDSHAKE_RATE": "2000"} {
		if !regexp.MustCompile(`name: ` + name + `\s+value: "` + val + `"`).MatchString(out) {
			t.Errorf("%s = %s missing", name, val)
		}
	}
	// D-1: no limit per source address is configurable any more.
	for _, gone := range []string{"DEMUX_MAX_ENTRIES_PER_SOURCE", "DEMUX_HANDSHAKE_RATE", "DEMUX_HANDSHAKE_BURST", "DEMUX_MAX_SOURCES", "DEMUX_OWNER_RATE", "DEMUX_OWNER_BURST"} {
		if strings.Contains(out, gone+"\n") || strings.Contains(out, "name: "+gone) {
			t.Errorf("%s must be gone: the source address is shared (NAT)", gone)
		}
	}
}
