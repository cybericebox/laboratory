package chart_test

import (
	"strings"
	"testing"
)

// The proxy runs two replicas by default, spread over nodes when there are several, with a budget that keeps
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
		"preferredDuringSchedulingIgnoredDuringExecution",
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
