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
		for _, old := range []string{"AGENT_LIMIT_DEVICE_", "AGENT_STATE_", "AGENT_SCHEDULER_", "AGENT_CACHE_", "AGENT_BASE_DOMAIN", "AGENT_PUBLIC_VPN_ENDPOINT", "AGENT_LAB_", "AGENT_PROXY_"} {
			if strings.HasPrefix(name, old) {
				t.Errorf("%s: the agent reads the operator's name for it", name)
			}
		}
	}
}

// Values that became constants of the images are refused when a stale values file still sets them: only ports inside the cluster
// network (the WireGuard port of the VPN pods, the ports of the proxy containers, the registry relay port).
func TestRemovedValuesAreRefused(t *testing.T) {
	for _, set := range []string{"operator.vpnServicePort=51821", "registry.forwardPort=5099",
		"proxy.l7.listen=:9443", "proxy.l7.healthPort=1", "proxy.wg.listenPort=1", "proxy.wg.healthPort=1"} {
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

// The tuning knobs stay values (and optional environment variables of the images): paths, names, namespaces, intervals, sizes.
func TestTuningKnobsStayValues(t *testing.T) {
	set := append(append([]string{}, agentSet...), "--set", "vpn.statsInterval=45s", "--set", "agent.namespace=lab-agent-x",
		"--set", "agent.monitoring.journalSize=77", "--set", "agent.monitoring.journalAge=3m", "--set", "agent.monitoring.pollInterval=2s",
		"--set", "agent.monitoring.subscriberBuffer=9", "--set", "agent.monitoring.maxStreamsPerTenant=3",
		"--set", "nodeAgent.ovsRunHostPath=/run/ovs-x", "--set", "nodeAgent.ovsSocket=/run/ovs-x/db.sock", "--set", "nodeAgent.ovsDBHostPath=/var/lib/ovs-x",
		"--set", "nodeAgent.grpcSocket=/run/cice/na.sock", "--set", "nodeAgent.ovsBridge=br-x", "--set", "nodeAgent.healthPort=9555",
		"--set", "nodeAgent.devicePlugin.dir=/var/lib/kubelet/dp-x", "--set", "priorityClasses.group.name=grp-x")
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, set...)
	for k, want := range map[string]string{"VPN_STATS_INTERVAL": "45s", "AGENT_SERVICE_NAMESPACE": "lab-agent-x", "AGENT_SERVICE_ACCOUNT": "laboratory-agent",
		"OPERATOR_SERVICE_ACCOUNT": "laboratory-controller-manager", "OPERATOR_NAMESPACE": "laboratory-system", "PRIORITY_CLASS_GROUP": "grp-x"} {
		if cm.Data[k] != want {
			t.Errorf("operator %s = %q, want %q", k, cm.Data[k], want)
		}
	}
	var dep appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &dep, set...)
	if dep.Namespace != "lab-agent-x" {
		t.Errorf("agent namespace %q, want lab-agent-x", dep.Namespace)
	}
	agentEnv := envOf(dep.Spec.Template.Spec.Containers[0])
	for k, want := range map[string]string{"AGENT_MONITORING_JOURNAL_SIZE": "77", "AGENT_MONITORING_JOURNAL_AGE": "3m", "AGENT_MONITORING_POLL_INTERVAL": "2s",
		"AGENT_MONITORING_SUBSCRIBER_BUFFER": "9", "AGENT_MONITORING_MAX_STREAMS_PER_TENANT": "3", "AGENT_RELEASE_NAMESPACE": "laboratory-system",
		"AGENT_PULL_SECRET_NAMESPACE": "laboratory-system", "AGENT_REGISTRY_ADDR": "laboratory-registry.laboratory-system.svc:5000"} {
		if agentEnv[k].Value != want {
			t.Errorf("agent %s = %q, want %q", k, agentEnv[k].Value, want)
		}
	}
	var ds appsv1.DaemonSet
	render(t, "templates/node-agent/daemonset.yaml", &ds, set...)
	var main corev1.Container
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == "node-agent" {
			main = c
		}
	}
	naEnv := envOf(main)
	for k, want := range map[string]string{"OVS_SOCK": "/run/ovs-x/db.sock", "GRPC_SOCK": "/run/cice/na.sock", "OVS_BRIDGE": "br-x",
		"HEALTH_ADDR": "127.0.0.1:9555", "DEVICE_PLUGIN_DIR": "/var/lib/kubelet/dp-x", "STATE_REGISTRY_ADDR": "laboratory-registry.laboratory-system.svc:5000"} {
		if naEnv[k].Value != want {
			t.Errorf("node-agent %s = %q, want %q", k, naEnv[k].Value, want)
		}
	}
	if main.StartupProbe == nil || main.StartupProbe.HTTPGet == nil || main.StartupProbe.HTTPGet.Port.IntValue() != 9555 {
		t.Errorf("the probes follow nodeAgent.healthPort: %+v", main.StartupProbe)
	}
	hostPaths := map[string]string{}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			hostPaths[v.Name] = v.HostPath.Path
		}
	}
	for _, p := range []string{"/run/ovs-x", "/var/lib/ovs-x", "/var/lib/kubelet/dp-x"} {
		found := false
		for _, got := range hostPaths {
			found = found || got == p
		}
		if !found {
			t.Errorf("no hostPath %s in %v", p, hostPaths)
		}
	}
	// the socket must stay inside the OVS run directory
	if out, err := helmTemplate(t, "--set", "nodeAgent.ovsSocket=/elsewhere/db.sock"); err == nil || !strings.Contains(out, "must be inside nodeAgent.ovsRunHostPath") {
		t.Errorf("an OVS socket outside its run directory must be refused: %v\n%s", err, out)
	}
}

// The defaults of all of them are unchanged by the knobs coming back.
func TestTuningKnobDefaults(t *testing.T) {
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, agentSet...)
	if cm.Data["VPN_STATS_INTERVAL"] != "30s" || cm.Data["AGENT_SERVICE_NAMESPACE"] != "laboratory-agent" || cm.Data["PRIORITY_CLASS_DEVICE"] != "laboratory-device" {
		t.Errorf("operator defaults: %v", cm.Data)
	}
	var ds appsv1.DaemonSet
	render(t, "templates/node-agent/daemonset.yaml", &ds, agentSet...)
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != "node-agent" {
			continue
		}
		env := envOf(c)
		if env["OVS_SOCK"].Value != "/run/openvswitch/db.sock" || env["OVS_BRIDGE"].Value != "br-ovs" || env["HEALTH_ADDR"].Value != "127.0.0.1:9440" ||
			env["GRPC_SOCK"].Value != "/run/cybericebox/node-agent.sock" || env["DEVICE_PLUGIN_DIR"].Value != "/var/lib/kubelet/device-plugins" {
			t.Errorf("node-agent defaults: %v", env)
		}
	}
}

// The port clients connect to OUTSIDE the cluster is configurable end to end: proxy.wg.publicPort is the port of the LoadBalancer
// Service and of the advertised address (PUBLIC_VPN_ENDPOINT, in the operator's and the agent's settings), and defaults to 51820.
// The port inside (the VPN pods, the demux, the Service's targetPort) is a constant.
func TestExternalWireGuardPort(t *testing.T) {
	endpointOf := func(t *testing.T, endpoint string, extra ...string) (string, int32, int32) {
		t.Helper()
		set := append([]string{"--set", "operator.publicVPNEndpoint=" + endpoint}, extra...)
		var cm corev1.ConfigMap
		render(t, "templates/operator/configmap.yaml", &cm, append(append([]string{}, agentSet...), set...)...)
		var svc corev1.Service
		render(t, "templates/proxy/service-wg-lb.yaml", &svc, append(append([]string{}, agentSet...), set...)...)
		var dep appsv1.Deployment
		render(t, "templates/agent/deployment.yaml", &dep, append(append([]string{}, agentSet...), set...)...)
		if got := envOf(dep.Spec.Template.Spec.Containers[0])["PUBLIC_VPN_ENDPOINT"].Value; got != cm.Data["PUBLIC_VPN_ENDPOINT"] {
			t.Errorf("the agent advertises %q, the operator %q", got, cm.Data["PUBLIC_VPN_ENDPOINT"])
		}
		if svc.Spec.Ports[0].TargetPort.IntValue() != 51820 {
			t.Errorf("the Service always forwards to the constant inner port 51820: %v", svc.Spec.Ports[0].TargetPort)
		}
		return cm.Data["PUBLIC_VPN_ENDPOINT"], svc.Spec.Ports[0].Port, int32(svc.Spec.Ports[0].TargetPort.IntValue())
	}
	// default: 51820, the port is appended to a bare host
	if ep, port, _ := endpointOf(t, "vpn.example.com"); ep != "vpn.example.com:51820" || port != 51820 {
		t.Errorf("default: endpoint %q, Service port %d", ep, port)
	}
	// the public port is a value
	if ep, port, _ := endpointOf(t, "vpn.example.com", "--set", "proxy.wg.publicPort=443"); ep != "vpn.example.com:443" || port != 443 {
		t.Errorf("publicPort=443: endpoint %q, Service port %d", ep, port)
	}
	// an endpoint that names its own port is advertised as it is (a front that maps the port); the Service port is still publicPort
	if ep, port, _ := endpointOf(t, "vpn.example.com:4000", "--set", "proxy.wg.publicPort=5000"); ep != "vpn.example.com:4000" || port != 5000 {
		t.Errorf("explicit endpoint port: endpoint %q, Service port %d", ep, port)
	}
	if out, err := helmTemplate(t, "--set", "proxy.wg.publicPort=70000"); err == nil || !strings.Contains(out, "proxy.wg.publicPort") {
		t.Errorf("a port above 65535 must be refused: %v\n%s", err, out)
	}
}
