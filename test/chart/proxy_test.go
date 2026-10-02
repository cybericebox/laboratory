package chart_test

import (
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
	out, err := helmTemplate(t, "--set", "proxy.mode=daemonset", "--set-string", "proxy.nodeSelector.lab=true",
		"-s", "templates/proxy/daemonset.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"kind: DaemonSet", "name: laboratory-proxy", "app: laboratory-proxy-l7", "nodeSelector:", "lab: \"true\"", "name: l7", "name: wg-demux"} {
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
