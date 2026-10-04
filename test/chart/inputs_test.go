package chart_test

import (
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// One release tag drives every image of the chart; a per-image tag still pins one of them.
func TestOneReleaseTagDrivesEveryImage(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "image.tag=1.2.3")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, repo := range []string{"laboratory-controller", "laboratory-agent", "laboratory-proxy", "laboratory-node", "laboratory-lab"} {
		images := regexp.MustCompile(`cybericebox/`+repo+`:(\S+?)"?\s`).FindAllStringSubmatch(out, -1)
		if len(images) == 0 {
			t.Errorf("no %s image rendered", repo)
		}
		for _, m := range images {
			if m[1] != "1.2.3" {
				t.Errorf("%s has the tag %s, the release tag is 1.2.3", repo, m[1])
			}
		}
	}
	// the default is the chart appVersion
	out, err = helmTemplate(t, agentSet...)
	if err != nil || !regexp.MustCompile(`cybericebox/laboratory-agent:0\.1\.0`).MatchString(out) {
		t.Errorf("the default tag is the appVersion: %v", err)
	}
	// a per-image tag wins for that image only
	out, err = helmTemplate(t, append(agentSet, "--set", "image.tag=1.2.3", "--set", "proxy.image.tag=sha-abc")...)
	if err != nil || !strings.Contains(out, "laboratory-proxy:sha-abc") || !strings.Contains(out, "laboratory-agent:1.2.3") {
		t.Errorf("per-image override: %v", err)
	}
	// a moving tag is refused, global or per image
	for _, bad := range [][]string{{"image.tag=latest"}, {"proxy.image.tag=latest"}} {
		if out, err := helmTemplate(t, "--set", bad[0]); err == nil {
			t.Errorf("%s must be refused:\n%s", bad[0], out)
		}
	}
}

// The VPN and the gateway are one image: the operator is told the gateway image only when it differs.
func TestGatewayImageIsPassedOnlyWhenItDiffers(t *testing.T) {
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, "--set", "image.tag=1.2.3")
	if cm.Data["VPN_IMAGE"] != "cybericebox/laboratory-lab:1.2.3" || cm.Data["NETCONFIG_IMAGE"] != "cybericebox/laboratory-node:1.2.3" {
		t.Errorf("images: %v", cm.Data)
	}
	if _, set := cm.Data["GATEWAY_IMAGE"]; set {
		t.Errorf("the gateway is the VPN image, nothing to pass: %q", cm.Data["GATEWAY_IMAGE"])
	}
	var other corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &other, "--set", "inetGateway.image.tag=sha-9")
	if other.Data["GATEWAY_IMAGE"] != "cybericebox/laboratory-lab:sha-9" {
		t.Errorf("a different gateway build is passed: %q", other.Data["GATEWAY_IMAGE"])
	}
}

// The agent's host, the lab domain and the VPN endpoint it reports come from the operator's values.
func TestAgentDomainAndEndpointAreDerived(t *testing.T) {
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "--set", "operator.baseDomain=labs.example.org", "-s", "templates/agent/certificate-server.yaml", "-s", "templates/agent/tlsroute.yaml")
	if err != nil || strings.Count(out, `"ctl.labs.example.org"`) != 2 {
		t.Errorf("agent.domain defaults to ctl.<baseDomain>: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "agent.enabled=true", "--set", "agent.domain=agent.example.org", "-s", "templates/agent/certificate-server.yaml")
	if err != nil || !strings.Contains(out, `"agent.example.org"`) {
		t.Errorf("agent.domain still overrides: %v\n%s", err, out)
	}
}

// One chart value, one name, written once: every variable that the operator and the agent both receive has the same value in both.
func TestOperatorAndAgentShareTheirSettings(t *testing.T) {
	set := append(append([]string{}, agentSet...), "--set", "devices.statePersistence.enabled=true", "--set", "registry.cache.enabled=true",
		"--set", "scheduler.maxPods=7", "--set", "limits.device.maxCpu=3", "--set", "imagePullSecrets[0].name=p")
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, set...)
	var dep appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &dep, set...)
	shared := 0
	for name, e := range envOf(dep.Spec.Template.Spec.Containers[0]) {
		if v, ok := cm.Data[name]; ok {
			shared++
			if e.Value != v {
				t.Errorf("%s: the operator gets %q, the agent %q", name, v, e.Value)
			}
		}
	}
	if shared < 25 {
		t.Errorf("only %d settings are shared by the operator and the agent", shared)
	}
	// and the agent has none of the second names it used to read
	for name := range envOf(dep.Spec.Template.Spec.Containers[0]) {
		for _, old := range []string{"AGENT_LIMIT_DEVICE_", "AGENT_STATE_", "AGENT_SCHEDULER_", "AGENT_CACHE_", "AGENT_BASE_DOMAIN", "AGENT_PUBLIC_VPN_ENDPOINT", "AGENT_LAB_", "AGENT_PROXY_", "AGENT_MONITORING_", "AGENT_PULL_SECRET"} {
			if strings.HasPrefix(name, old) {
				t.Errorf("%s: the agent reads the operator's name for it", name)
			}
		}
	}
}

// Values that became constants of the images are refused when a stale values file still sets them.
func TestRemovedValuesAreRefused(t *testing.T) {
	for _, set := range []string{"operator.vpnServicePort=51821", "vpn.statsInterval=1m", "registry.forwardPort=5099", "agent.namespace=other", "agent.monitoring.journalSize=1",
		"proxy.l7.listen=:9443", "proxy.l7.healthPort=1", "proxy.wg.listenPort=1", "proxy.wg.healthPort=1", "nodeAgent.ovsBridge=x", "nodeAgent.grpcSocket=/x",
		"nodeAgent.healthPort=1", "nodeAgent.ovsRunHostPath=/x", "nodeAgent.ovsDBHostPath=/x", "priorityClasses.group.name=x"} {
		if out, err := helmTemplate(t, "--set", set); err == nil {
			t.Errorf("%s must be refused:\n%s", set, out)
		} else if !strings.Contains(out, "removed") {
			t.Errorf("%s: the error must say it was removed:\n%s", set, out)
		}
	}
}

// The constants of the platform appear in the chart exactly where the images expect them.
func TestPlatformConstantsInTheChart(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/service-wg-lb.yaml", "-s", "templates/proxy/service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`port: 51820\s+targetPort: 51820`).MatchString(out) || !strings.Contains(out, "targetPort: 8443") {
		t.Errorf("the WireGuard port 51820 and the HTTPS port 8443:\n%s", out)
	}
}
