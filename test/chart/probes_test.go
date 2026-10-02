package chart_test

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func probeOf(t *testing.T, c corev1.Container) (startup, ready, live *corev1.Probe) {
	t.Helper()
	return c.StartupProbe, c.ReadinessProbe, c.LivenessProbe
}

func needProbes(t *testing.T, who string, c corev1.Container) {
	t.Helper()
	s, r, l := probeOf(t, c)
	if s == nil || r == nil || l == nil {
		t.Errorf("%s: needs a startup, a readiness and a liveness probe: %v %v %v", who, s != nil, r != nil, l != nil)
	}
}

// E-6: the agent, the L7 proxy, wg-demux, the node-agent and the operator say when they really serve; rollouts wait for it.
func TestEveryServiceHasProbesAndRollsOutOnReadiness(t *testing.T) {
	agentArgs := []string{"--set", "agent.enabled=true", "--set", "agent.domain=a.example.com"}

	var agent appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &agent, agentArgs...)
	needProbes(t, "agent", agent.Spec.Template.Spec.Containers[0])
	if p := agent.Spec.Template.Spec.Containers[0].ReadinessProbe; p == nil || p.TCPSocket == nil {
		t.Errorf("the agent's readiness is its bound mTLS port: %+v", p)
	}
	st := agent.Spec.Strategy.RollingUpdate
	if st == nil || st.MaxUnavailable.IntValue() != 0 || st.MaxSurge.IntValue() != 1 || agent.Spec.MinReadySeconds == 0 {
		t.Errorf("the agent rolls out one pod at a time, never below its replicas, and waits for Ready: %+v %d", st, agent.Spec.MinReadySeconds)
	}
	if g := agent.Spec.Template.Spec.TerminationGracePeriodSeconds; g == nil || *g < 35 {
		t.Errorf("the agent needs time to finish calls (shutdown grace 30s plus the preStop sleep): %v", g)
	}
	if lc := agent.Spec.Template.Spec.Containers[0].Lifecycle; lc == nil || lc.PreStop == nil || lc.PreStop.Sleep == nil {
		t.Error("the agent sleeps in preStop so the Service forgets it first")
	}
	if envOf(agent.Spec.Template.Spec.Containers[0])["AGENT_SHUTDOWN_GRACE"].Value != "30s" {
		t.Error("AGENT_SHUTDOWN_GRACE")
	}

	var proxy appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &proxy)
	byName := map[string]corev1.Container{}
	for _, c := range proxy.Spec.Template.Spec.Containers {
		byName[c.Name] = c
	}
	for name, port := range map[string]int32{"l7": 8081, "wg-demux": 8082} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("no %s container", name)
		}
		needProbes(t, name, c)
		if c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.LivenessProbe.HTTPGet.Path != "/healthz" {
			t.Errorf("%s: readiness /readyz, liveness /healthz: %+v %+v", name, c.ReadinessProbe, c.LivenessProbe)
		}
		var declared int32
		for _, p := range c.Ports {
			if p.Name == "health" {
				declared = p.ContainerPort
			}
		}
		if declared != port || envOf(c)["HEALTH_ADDR"].Value != fmt.Sprintf(":%d", port) {
			t.Errorf("%s: the health port %d and HEALTH_ADDR must agree: %d %q", name, port, declared, envOf(c)["HEALTH_ADDR"].Value)
		}
	}
	if proxy.Spec.MinReadySeconds == 0 {
		t.Error("the proxy waits for the new pod to stay Ready before replacing the next")
	}
	// the Deployment cannot surge a pod when it cannot count the nodes (the anti-affinity is required): one at a time, held back by readiness
	if r := proxy.Spec.Strategy.RollingUpdate; r.MaxSurge.IntValue() != 0 || r.MaxUnavailable.IntValue() != 1 {
		t.Errorf("without a node count the proxy replaces one pod at a time: %+v", r)
	}
	var surging appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &surging, "--set", "proxy.surge=true")
	if r := surging.Spec.Strategy.RollingUpdate; r.MaxSurge.IntValue() != 1 || r.MaxUnavailable.IntValue() != 0 {
		t.Errorf("proxy.surge=true: maxSurge 1, maxUnavailable 0: %+v", r)
	}

	var ds appsv1.DaemonSet
	render(t, "templates/node-agent/daemonset.yaml", &ds)
	var main corev1.Container
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == "node-agent" {
			main = c
		}
	}
	needProbes(t, "node-agent", main)
	if main.LivenessProbe.HTTPGet == nil || main.LivenessProbe.HTTPGet.Host != "127.0.0.1" || main.LivenessProbe.HTTPGet.Port.IntValue() != 9440 ||
		envOf(main)["HEALTH_ADDR"].Value != "127.0.0.1:9440" {
		t.Errorf("the node-agent's probes are on the loopback of the node, where it listens: %+v %q", main.LivenessProbe.HTTPGet, envOf(main)["HEALTH_ADDR"].Value)
	}
	if ds.Spec.MinReadySeconds == 0 || ds.Spec.UpdateStrategy.RollingUpdate == nil || ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntValue() != 1 {
		t.Errorf("a node-agent rollout moves one node at a time and waits for Ready: %+v", ds.Spec.UpdateStrategy)
	}

	out, err := helmTemplate(t, "-s", "templates/operator/deployment.yaml")
	if err != nil || !strings.Contains(out, "readinessProbe") || !strings.Contains(out, "livenessProbe") {
		t.Errorf("the operator has probes: %v", err)
	}
}
