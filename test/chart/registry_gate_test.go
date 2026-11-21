package chart_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func registryPod(t *testing.T, extra ...string) corev1.PodSpec {
	t.Helper()
	var dep appsv1.Deployment
	render(t, "templates/registry/deployment.yaml", &dep, append([]string{"--set", statePath + "enabled=true"}, extra...)...)
	return dep.Spec.Template.Spec
}

func containerNamed(spec corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

// Every request to the registry passes the gate: the Service's one port is the gate's TLS port, and zot listens on the loopback of the pod only.
func TestRegistryGateIsTheOnlyWayToZot(t *testing.T) {
	spec := registryPod(t)
	zot, gate := containerNamed(spec, "zot"), containerNamed(spec, "gate")
	if zot == nil || gate == nil {
		t.Fatalf("the registry pod has a zot and a gate container, got %+v", spec.Containers)
	}
	if len(zot.Ports) != 0 {
		t.Errorf("zot declares no port: it is not reachable but through the gate, %+v", zot.Ports)
	}
	if zot.ReadinessProbe != nil || zot.LivenessProbe != nil {
		t.Errorf("a loopback-only zot cannot be probed by the kubelet; the gate's readiness follows it")
	}
	var svc corev1.Service
	render(t, "templates/registry/service.yaml", &svc, "--set", statePath+"enabled=true")
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5443 || svc.Spec.Ports[0].TargetPort.StrVal != "registry-tls" {
		t.Fatalf("the Service has the gate's TLS port only: %+v", svc.Spec.Ports)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("the Service is a NodePort with Local traffic: %s %s", svc.Spec.Type, svc.Spec.ExternalTrafficPolicy)
	}
	var fixed corev1.Service
	render(t, "templates/registry/service.yaml", &fixed, "--set", statePath+"enabled=true", "--set", "registry.gate.service.nodePort=30543",
		"--set", "registry.gate.service.annotations.lb\\.example\\.com/internal=true")
	if fixed.Spec.Ports[0].NodePort != 30543 || fixed.Annotations["lb.example.com/internal"] != "true" {
		t.Errorf("the node port and the annotations are values: %+v %v", fixed.Spec.Ports[0], fixed.Annotations)
	}
	var named []string
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if p.Name == "registry-tls" {
				named = append(named, c.Name)
				if p.ContainerPort != 5443 {
					t.Errorf("the registry port of %s is %d", c.Name, p.ContainerPort)
				}
			}
			if p.ContainerPort == 5000 {
				t.Errorf("%s: the plain port is gone, a token must not cross the network in the clear", c.Name)
			}
		}
	}
	if len(named) != 1 || named[0] != "gate" {
		t.Errorf("only the gate carries the port the Service targets, got %v", named)
	}

	var cm corev1.ConfigMap
	render(t, "templates/registry/configmap.yaml", &cm, "--set", statePath+"enabled=true")
	var cfg struct {
		HTTP struct{ Address, Port string } `json:"http"`
	}
	if err := json.Unmarshal([]byte(cm.Data["config.json"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Address != "127.0.0.1" || cfg.HTTP.Port != "5001" {
		t.Errorf("zot listens on %s:%s, want 127.0.0.1:5001", cfg.HTTP.Address, cfg.HTTP.Port)
	}
	vars := envOf(*gate)
	if vars["GATE_ZOT_ADDR"].Value != cfg.HTTP.Address+":"+cfg.HTTP.Port {
		t.Errorf("the gate dials %q, zot listens on %s:%s", vars["GATE_ZOT_ADDR"].Value, cfg.HTTP.Address, cfg.HTTP.Port)
	}
	if _, set := vars["GATE_LISTEN"]; set || vars["GATE_TLS_LISTEN"].Value != ":5443" {
		t.Errorf("the gate has one listener, TLS on 5443: %v %v", vars["GATE_LISTEN"], vars["GATE_TLS_LISTEN"])
	}
}

func TestRegistryGateContainer(t *testing.T) {
	spec := registryPod(t, "--set", "registry.gate.resources.limits.memory=300Mi")
	gate := containerNamed(spec, "gate")
	if len(gate.Command) != 2 || gate.Command[0] != "/node" || gate.Command[1] != "registry-gate" {
		t.Errorf("command %v", gate.Command)
	}
	if want := nodeAgent(t).Spec.Template.Spec.Containers[0].Image; gate.Image != want {
		t.Errorf("the gate runs the node image %s, got %s", want, gate.Image)
	}
	sc := gate.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("the gate is a plain user with nothing: %+v", sc)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Errorf("the pod runs as non-root")
	}
	if gate.ReadinessProbe == nil || gate.ReadinessProbe.HTTPGet.Path != "/readyz" || gate.LivenessProbe == nil || gate.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("probes %+v %+v", gate.ReadinessProbe, gate.LivenessProbe)
	}
	if got := gate.Resources.Limits.Memory().String(); got != "300Mi" {
		t.Errorf("registry.gate.resources reaches the container: %s", got)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range gate.VolumeMounts {
		mounts[m.Name] = m
	}
	for _, name := range []string{"gate-config", "gate-secret", "gate-tls", "data"} {
		if !mounts[name].ReadOnly {
			t.Errorf("the gate mounts %s read-only: %+v", name, mounts[name])
		}
	}
	for _, v := range spec.Volumes {
		switch v.Name {
		case "gate-secret":
			var keys []string
			for _, it := range v.Secret.Items {
				keys = append(keys, it.Key)
			}
			if strings.Join(keys, ",") != "gatePassword,gateKey" {
				t.Errorf("the gate reads its own account and key and nothing else of the registry Secret: %v", keys)
			}
		case "gate-tls":
			var keys []string
			for _, it := range v.Secret.Items {
				keys = append(keys, it.Key)
			}
			if strings.Join(keys, ",") != "tls.crt,tls.key" {
				t.Errorf("the gate reads its certificate and key, not the authority: %v", keys)
			}
		}
	}
	zot := containerNamed(spec, "zot")
	for _, m := range zot.VolumeMounts {
		if m.Name == "gate-secret" || m.Name == "gate-config" || m.Name == "gate-tls" {
			t.Errorf("zot must not see the gate's key: %+v", m)
		}
	}
	var sec corev1.Secret
	render(t, "templates/registry/secret.yaml", &sec)
	if sec.StringData["gatePassword"] == "" || sec.StringData["gateKey"] == "" || !strings.Contains(sec.StringData["htpasswd"], "gate:$2") || len(sec.StringData) != 3 {
		t.Errorf("the registry Secret carries the gate's account and key and nothing else: %d entries", len(sec.StringData))
	}
}

// The gate is not rendered without the registry.
func TestRegistryGateIsNotRenderedWithoutTheRegistry(t *testing.T) {
	out, err := helmTemplate(t, "--set", "registry.cache.enabled=false", "--set", "nodeAgent.enabled=false")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		if name, _ := md["name"].(string); strings.HasPrefix(name, "laboratory-registry") {
			t.Errorf("%v/%s without a registry", d["kind"], name)
		}
	}
}

// The wiring of the gate: how a caller is known (the audience of the token and the accounts that may call), the cache (always on), the limits of a
// pull and of a push, and requireDigest (off by default).
func TestRegistryGateWiring(t *testing.T) {
	gate := containerNamed(registryPod(t, "--set", "agent.enabled=true", "--set", "registry.cache.staleIfError=7m"), "gate")
	vars := envOf(*gate)
	for k, want := range map[string]string{
		"GATE_CACHE_ENABLED": "true", "GATE_STALE_IF_ERROR": "7m", "GATE_DECISION_MAX_AGE": "2m", "GATE_DENIED_FOR": "30s", "GATE_DISK_HIGH_WATER": "90",
		"GATE_IMAGE_HOST": "localhost:5035", "GATE_SNAPSHOTS_ENABLED": "true",
		"GATE_OPERATOR_ACCOUNT": "laboratory-system/laboratory-controller-manager", "GATE_NODE_ACCOUNT": "laboratory-system/laboratory-node-agent",
		"GATE_AGENT_ACCOUNT": "laboratory-agent/laboratory-agent", "GATE_REQUIRE_DIGEST": "false", "GATE_BIND_SOURCE": "true",
		"GATE_TOKEN_CACHE_TTL": "5m", "GATE_PULL_GRACE": "2m", "GATE_GRANT_MAX_AGE": "1h", "GATE_API_STALE_MAX": "15m", "GATE_AGENT_MAX_FILLS": "8",
	} {
		if vars[k].Value != want {
			t.Errorf("%s = %q, want %q", k, vars[k].Value, want)
		}
	}
	// the gate refuses a registry that is not on registry.allowedRegistries before any upstream call: the same list as the operator's
	if got := vars["GATE_ALLOWED_REGISTRIES"].Value; !strings.Contains(","+got+",", ",ghcr.io,") || strings.Contains(got, "*") {
		t.Errorf("GATE_ALLOWED_REGISTRIES = %q: want the chart's allowedRegistries", got)
	}
	for _, e := range gate.Env {
		if e.ValueFrom != nil {
			t.Errorf("%s comes from a Secret or a field: the gate holds its settings as plain values", e.Name)
		}
	}
	// requireDigest reaches the gate and the operator
	on := envOf(*containerNamed(registryPod(t, "--set", "registry.cache.requireDigest=true"), "gate"))
	if on["GATE_REQUIRE_DIGEST"].Value != "true" || operatorConfig(t, "--set", "registry.cache.requireDigest=true")["IMAGE_CACHE_REQUIRE_DIGEST"] != "true" {
		t.Errorf("requireDigest: gate %q", on["GATE_REQUIRE_DIGEST"].Value)
	}
	if operatorConfig(t)["IMAGE_CACHE_REQUIRE_DIGEST"] != "false" {
		t.Error("requireDigest is off by default")
	}
	// the push rules are the gate's, not node-state's: rendered only with state persistence
	for _, k := range []string{"GATE_MIN_PUSH_INTERVAL", "GATE_PUSH_BUDGET_BYTES", "GATE_PUSH_BUDGET_WINDOW", "GATE_TENANT_QUOTA_BYTES", "GATE_REGISTRY_RESERVE", "GATE_SUPERSEDED_GRACE", "GATE_EXIT_MAX_BYTES", "GATE_EXIT_PUSHES_PER_HOUR"} {
		if vars[k].Value == "" {
			t.Errorf("%s is missing with state persistence on", k)
		}
	}
	if vars["GATE_MIN_PUSH_INTERVAL"].Value != "60s" || vars["GATE_PUSH_BUDGET_BYTES"].Value != "2147483648" || vars["GATE_TENANT_QUOTA_BYTES"].Value != "10737418240" || vars["GATE_EXIT_PUSHES_PER_HOUR"].Value != "6" || vars["GATE_REGISTRY_RESERVE"].Value != "0.1" {
		t.Errorf("the push rules do not carry the chart's values")
	}
	noState := envOf(*containerNamed(registryPod(t, "--set", statePath+"enabled=false"), "gate"))
	if _, set := noState["GATE_PUSH_BUDGET_BYTES"]; set || noState["GATE_SNAPSHOTS_ENABLED"].Value != "" {
		t.Error("push rules without state persistence")
	}
	state := envOf(nodeState(t, "--set", statePath+"enabled=true").Spec.Template.Spec.Containers[0])
	for _, k := range []string{"STATE_MIN_PUSH_INTERVAL", "STATE_PUSH_BUDGET", "STATE_PUSH_BUDGET_WINDOW", "STATE_SUPERSEDED_GRACE", "STATE_REGISTRY_CAPACITY", "STATE_REGISTRY_RESERVE"} {
		if _, set := state[k]; set {
			t.Errorf("node-state still gets %s: the gate measures the pushes now", k)
		}
	}
}

// The TLS hop is the only hop: the gate's certificate names the Service and the address the nodes dial (a DNS name or an IP address), the authority is
// published as a ConfigMap (public data) in the release namespace and the agent's, and no node-side object holds the Secret that has the key.
func TestRegistryGateTLSHop(t *testing.T) {
	for _, tc := range []struct {
		addr string
		dns  string
		ip   string
	}{
		{"registry.lab.internal:5443", "registry.lab.internal", ""},
		{"10.20.30.40:5443", "", "10.20.30.40"},
	} {
		out, err := helmTemplate(t, "--set", "registry.gate.address="+tc.addr, "--set", "agent.enabled=true", "-s", "templates/registry/tls-secret.yaml")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		var sec corev1.Secret
		cas := map[string]string{}
		for _, d := range docs(t, out) {
			raw, _ := yaml.Marshal(d)
			switch d["kind"] {
			case "Secret":
				if err := yaml.Unmarshal(raw, &sec); err != nil {
					t.Fatal(err)
				}
			case "ConfigMap":
				var cm corev1.ConfigMap
				if err := yaml.Unmarshal(raw, &cm); err != nil {
					t.Fatal(err)
				}
				cas[cm.Namespace+"/"+cm.Name] = cm.Data["ca.crt"]
			}
		}
		for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
			if !strings.Contains(sec.StringData[k], "-----BEGIN") {
				t.Errorf("the TLS Secret holds %s", k)
			}
		}
		for _, ns := range []string{"laboratory-system", "laboratory-agent"} {
			if cas[ns+"/laboratory-registry-ca"] != sec.StringData["ca.crt"] || cas[ns+"/laboratory-registry-ca"] == "" {
				t.Errorf("%s: the authority is published in a ConfigMap", ns)
			}
		}
		block, _ := pem.Decode([]byte(sec.StringData["tls.crt"]))
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"laboratory-registry", "laboratory-registry.laboratory-system.svc"} {
			if !has(cert.DNSNames, name) {
				t.Errorf("%s: the certificate lacks %s: %v", tc.addr, name, cert.DNSNames)
			}
		}
		if tc.dns != "" && !has(cert.DNSNames, tc.dns) {
			t.Errorf("%s: the certificate lacks the DNS name: %v", tc.addr, cert.DNSNames)
		}
		var ips []string
		for _, ip := range cert.IPAddresses {
			ips = append(ips, ip.String())
		}
		if tc.ip != "" && !has(ips, tc.ip) {
			t.Errorf("%s: the certificate lacks the IP address: %v", tc.addr, ips)
		}
		if tc.ip == "" && len(ips) != 0 {
			t.Errorf("%s: IP addresses %v", tc.addr, ips)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(sec.StringData["ca.crt"]))
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "laboratory-registry.laboratory-system.svc"}); err != nil {
			t.Errorf("%s: the certificate is not signed by the published authority: %v", tc.addr, err)
		}
	}

	// the node-agent dials the address of the load balancer with the node's own token, and mounts the authority (a ConfigMap) and nothing secret
	agent := nodeAgent(t, "--set", "registry.gate.address=reg.example.com:7443").Spec.Template.Spec
	vars := envOf(agent.Containers[0])
	for k, want := range map[string]string{"STATE_REGISTRY_ADDR": "reg.example.com:7443", "STATE_REGISTRY_CA_FILE": "/etc/registry-ca/ca.crt",
		"REGISTRY_TOKEN_FILE": "/var/run/secrets/registry-gate/token"} {
		if vars[k].Value != want {
			t.Errorf("%s = %q, want %q", k, vars[k].Value, want)
		}
	}
	var sawCA, sawToken bool
	for _, v := range agent.Volumes {
		if v.Secret != nil {
			t.Errorf("the node-agent mounts the Secret %s: a node reads every Secret a pod bound to it mounts", v.Secret.SecretName)
		}
		switch v.Name {
		case "registry-ca":
			sawCA = v.ConfigMap != nil && v.ConfigMap.Name == "laboratory-registry-ca"
		case "registry-gate-token":
			if v.Projected != nil && len(v.Projected.Sources) == 1 && v.Projected.Sources[0].ServiceAccountToken != nil {
				tok := v.Projected.Sources[0].ServiceAccountToken
				sawToken = tok.Audience == "laboratory-registry-gate" && tok.Path == "token" && tok.ExpirationSeconds != nil && *tok.ExpirationSeconds == 3600
			}
		}
	}
	if !sawCA || !sawToken {
		t.Errorf("the node-agent mounts the authority (a ConfigMap) %v and a projected token for the gate %v", sawCA, sawToken)
	}
	// the operator and the agent trust the same authority and show their own token
	for _, tc := range []struct{ tpl, kind string }{{"templates/operator/deployment.yaml", "operator"}, {"templates/agent/deployment.yaml", "agent"}} {
		var dep appsv1.Deployment
		render(t, tc.tpl, &dep, "--set", "agent.enabled=true")
		var ca, token bool
		for _, v := range dep.Spec.Template.Spec.Volumes {
			ca = ca || v.Name == "registry-ca" && v.ConfigMap != nil && v.ConfigMap.Name == "laboratory-registry-ca"
			token = token || v.Name == "registry-gate-token" && v.Projected != nil && v.Projected.Sources[0].ServiceAccountToken.Audience == "laboratory-registry-gate"
		}
		if !ca || !token {
			t.Errorf("%s: the authority %v and the token %v", tc.kind, ca, token)
		}
	}
	// no workload but the registry pod mounts the Secret that has the key
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "--set", "terminal.enabled=true", "--set", "checks.enabled=true", "--set", statePath+"enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "Deployment" && kind != "DaemonSet" {
			continue
		}
		raw, _ := yaml.Marshal(d)
		if strings.Contains(string(raw), "secretName: laboratory-registry-tls") && d["metadata"].(map[string]any)["name"] != "laboratory-registry" {
			t.Errorf("%s/%v mounts the Secret that holds the key of the gate", kind, d["metadata"].(map[string]any)["name"])
		}
	}
}

// What the gate may do in the cluster: check tokens, watch what it decides from, read the upstream credentials in their own namespace, write nothing.
func TestRegistryGateRights(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/registry/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var clusterRules, roleRules []rule
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		switch d["kind"] {
		case "ClusterRole":
			clusterRules = rulesOf(t, d)
		case "Role":
			if md["namespace"] != "laboratory-upstream" {
				t.Errorf("the Role is in %v", md["namespace"])
			}
			roleRules = rulesOf(t, d)
		case "ServiceAccount":
			if md["name"] != "laboratory-registry-gate" {
				t.Errorf("service account %v", md["name"])
			}
		}
	}
	for _, r := range append(append([]rule{}, clusterRules...), roleRules...) {
		for _, v := range r.Verbs {
			if v != "get" && v != "list" && v != "watch" && (v != "create" || !has(r.Resources, "tokenreviews")) {
				t.Errorf("a write right: %+v", r)
			}
		}
	}
	for _, r := range clusterRules {
		if has(r.Resources, "secrets") || has(r.Resources, "configmaps") {
			t.Errorf("a cluster-wide right on credentials: %+v", r)
		}
	}
	if len(roleRules) != 1 || !has(roleRules[0].Resources, "secrets") || len(roleRules[0].Resources) != 1 {
		t.Errorf("one namespaced rule, on Secrets: %+v", roleRules)
	}
	need := map[string]bool{"tokenreviews": false, "pods": false, "nodes": false, "devices": false, "labs": false, "imagepulls": false, "tenants": false, "labgroups": false}
	for _, r := range clusterRules {
		for _, res := range r.Resources {
			if _, ok := need[res]; !ok {
				t.Errorf("unexpected resource %s", res)
				continue
			}
			need[res] = true
			if res != "tokenreviews" && (!has(r.Verbs, "get") || !has(r.Verbs, "list") || !has(r.Verbs, "watch")) {
				t.Errorf("%s needs get, list, watch: %+v", res, r)
			}
		}
	}
	for res, ok := range need {
		if !ok {
			t.Errorf("the gate lacks its right on %s", res)
		}
	}
}

// The registry's network policy has one port, 5443, for the nodes, the operator and the agent, and nothing from the platform.
func TestRegistryNetworkPolicyIsTheGatePortOnly(t *testing.T) {
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "-s", "templates/netpol/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"5000"`) || strings.Contains(out, "writer") {
		t.Errorf("the plain port or a writer is left:\n%s", out)
	}
	var pol struct {
		Spec struct {
			Ingress []struct {
				FromEntities  []string `json:"fromEntities"`
				FromEndpoints []struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"fromEndpoints"`
				ToPorts []struct {
					Ports []struct{ Port string } `json:"ports"`
				} `json:"toPorts"`
			} `json:"ingress"`
		} `json:"spec"`
	}
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.Contains(d, "kind: CiliumNetworkPolicy") {
			if err := yaml.Unmarshal([]byte(d), &pol); err != nil {
				t.Fatal(err)
			}
		}
	}
	var sawNodes, sawOperator, sawAgent bool
	for _, in := range pol.Spec.Ingress {
		if len(in.ToPorts) != 1 || len(in.ToPorts[0].Ports) != 1 || in.ToPorts[0].Ports[0].Port != "5443" {
			t.Errorf("a rule that is not the gate's TLS port alone: %+v", in)
		}
		sawNodes = sawNodes || strings.Join(in.FromEntities, ",") == "host,remote-node"
		for _, e := range in.FromEndpoints {
			ns := e.MatchLabels["k8s:io.kubernetes.pod.namespace"]
			switch {
			case e.MatchLabels["control-plane"] == "controller-manager":
				sawOperator = true
			case ns == "laboratory-agent" && e.MatchLabels["app"] == "laboratory-agent":
				sawAgent = true
			default:
				t.Errorf("a caller that is neither the operator nor the agent: %+v", e)
			}
		}
	}
	if !sawNodes || !sawOperator || !sawAgent {
		t.Errorf("nodes %v, operator %v, agent %v", sawNodes, sawOperator, sawAgent)
	}
}

// The cache is the only way images reach a lab node, so it is not optional with lab nodes; the nodes dial an address of the internal load balancer,
// which the chart needs before it installs the registry.
func TestLabNodesNeedTheCacheAndTheGateAddress(t *testing.T) {
	if out, err := helmTemplate(t, "--set", "registry.cache.enabled=false"); err == nil || !strings.Contains(out, "nodeAgent.enabled needs registry.cache.enabled") {
		t.Errorf("a lab node without the cache: %v\n%s", err, out)
	}
	if out, err := helmTemplate(t, "--set", "registry.cache.enabled=false", "--set", "nodeAgent.enabled=false"); err != nil {
		t.Errorf("no lab nodes, no cache: %v\n%s", err, out)
	}
	for _, bad := range []string{"", "registry.lab.internal", "registry.lab.internal:", "http://x:5443", "a b:5443"} {
		if out, err := helmTemplate(t, "--set", "registry.gate.address="+bad); err == nil {
			t.Errorf("address %q was accepted:\n%s", bad, out)
		}
	}
	// the default install, with only what infrastructure sets, is accepted
	if out, err := helmTemplate(t); err != nil {
		t.Errorf("%v\n%s", err, out)
	}
}

// The gate of the kind E2E: the chart is rendered with the exact flags of hack/registry-gate-e2e/run.sh, and the allowed list that the gate
// gets holds the two registries that the test uses and not the third one it knows (so the refusal is tested end to end).
func TestRegistryGateAllowedListWithTheE2EFlags(t *testing.T) {
	spec := registryPod(t,
		"--set", "registry.cache.enabled=true", "--set-json", `registry.cache.registries=["docker.io"]`, // docker.io: the platform images
		"--set-json", `registry.allowedRegistries=["pub","priv"]`,
		"--set", "registry.cache.extraRegistries[0].name=pub", "--set", "registry.cache.extraRegistries[0].url=http://upstream-pub.laboratory-system.svc:5000",
		"--set", "registry.cache.extraRegistries[1].name=priv", "--set", "registry.cache.extraRegistries[1].url=http://upstream-priv.laboratory-system.svc:5000",
		"--set", "registry.cache.extraRegistries[2].name=off", "--set", "registry.cache.extraRegistries[2].url=http://upstream-pub.laboratory-system.svc:5000",
	)
	got := envOf(*containerNamed(spec, "gate"))["GATE_ALLOWED_REGISTRIES"].Value
	list := "," + got + ","
	for _, want := range []string{",pub,", ",priv,"} {
		if !strings.Contains(list, want) {
			t.Errorf("GATE_ALLOWED_REGISTRIES = %q misses %s", got, want)
		}
	}
	if strings.Contains(list, ",off,") || strings.Contains(got, "*") || got == "" {
		t.Errorf("GATE_ALLOWED_REGISTRIES = %q: the gate must get a list that leaves the third registry out", got)
	}
}

// With the image cache on every image goes through it, so every allowed registry (and the registry of every platform image) must be one
// the cache serves, and "*" cannot be served: the chart refuses the install instead of leaving a registry that would be pulled directly.
func TestCacheOnRefusesRegistriesItDoesNotServe(t *testing.T) {
	cache := []string{"--set", "registry.cache.enabled=true"}
	for name, tc := range map[string]struct {
		extra []string
		fail  string
	}{
		"default list":      {nil, ""},
		"unserved registry": {[]string{"--set-json", `registry.allowedRegistries=["docker.io","gcr.io"]`}, `registry "gcr.io"`},
		"any registry":      {[]string{"--set-json", `registry.allowedRegistries=["*"]`}, `"*" cannot be used`},
		"served by extra": {[]string{"--set-json", `registry.allowedRegistries=["docker.io","reg.example.com:5000"]`,
			"--set", "registry.cache.extraRegistries[0].name=reg.example.com:5000", "--set", "registry.cache.extraRegistries[0].url=https://reg.example.com:5000"}, ""},
		"platform image off the list": {[]string{"--set", "vpn.image.repository=private.example.com/vpn"}, `registry "private.example.com"`},
	} {
		out, err := helmTemplate(t, append(append([]string{}, cache...), tc.extra...)...)
		switch {
		case tc.fail == "" && err != nil:
			t.Errorf("%s: %v\n%s", name, err, out)
		case tc.fail != "" && (err == nil || !strings.Contains(out, tc.fail)):
			t.Errorf("%s: want a refusal with %q, got %v\n%s", name, tc.fail, err, out)
		}
	}
	// with the cache off (and no lab node) the list alone decides, "*" included
	if _, err := helmTemplate(t, "--set", "registry.cache.enabled=false", "--set", "nodeAgent.enabled=false", "--set-json", `registry.allowedRegistries=["*"]`); err != nil {
		t.Errorf("cache off: %v", err)
	}
}
