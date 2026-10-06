package chart_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/proxy"

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
		// The listen address is the image's default (proxy.LoadL7Config / LoadWGConfig), so the chart passes none and must name that port.
		if _, set := envOf(c)["HEALTH_ADDR"]; set || declared != port || c.ReadinessProbe.HTTPGet.Port.String() != "health" {
			t.Errorf("%s: the health port %d must be the image default (no HEALTH_ADDR passed): %d %v", name, port, declared, envOf(c)["HEALTH_ADDR"])
		}
		if got := imageHealthAddr(t, name); got != fmt.Sprintf(":%d", port) {
			t.Errorf("%s: the image listens for probes on %q, the chart probes %d", name, got, port)
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
	// The node-agent listens on the loopback of the node at nodeAgent.healthPort (9440): HEALTH_ADDR and the probes agree.
	if envOf(main)["HEALTH_ADDR"].Value != "127.0.0.1:9440" || main.LivenessProbe.HTTPGet == nil || main.LivenessProbe.HTTPGet.Host != "127.0.0.1" || main.LivenessProbe.HTTPGet.Port.IntValue() != 9440 {
		t.Errorf("the node-agent's probes are on the loopback of the node, at its health port: %+v %v", main.LivenessProbe.HTTPGet, envOf(main)["HEALTH_ADDR"])
	}
	if ds.Spec.MinReadySeconds == 0 || ds.Spec.UpdateStrategy.RollingUpdate == nil || ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntValue() != 1 {
		t.Errorf("a node-agent rollout moves one node at a time and waits for Ready: %+v", ds.Spec.UpdateStrategy)
	}

	out, err := helmTemplate(t, "-s", "templates/operator/deployment.yaml")
	if err != nil || !strings.Contains(out, "readinessProbe") || !strings.Contains(out, "livenessProbe") {
		t.Errorf("the operator has probes: %v", err)
	}
}

// imageHealthAddr is where a proxy container of the image listens for its probes when nothing is passed: the l7 or the wg-demux default.
func imageHealthAddr(t *testing.T, container string) string {
	t.Helper()
	if container == "l7" {
		t.Setenv("BASE_DOMAIN", "example.com")
		t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
		cfg, err := proxy.LoadL7Config()
		if err != nil {
			t.Fatal(err)
		}
		return cfg.HealthAddr
	}
	cfg, err := proxy.LoadWGConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.HealthAddr
}
