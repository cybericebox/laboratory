package chart_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var agentSet = []string{"--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com"}

// docs splits helm output into its YAML documents, decoded generically.
func docs(t *testing.T, out string) []map[string]any {
	t.Helper()
	var res []map[string]any
	for _, d := range strings.Split(out, "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatalf("decode: %v\n%s", err, d)
		}
		if m != nil {
			res = append(res, m)
		}
	}
	return res
}

func ofKind(all []map[string]any, kind string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, d := range all {
		if d["kind"] == kind {
			md, _ := d["metadata"].(map[string]any)
			out[md["name"].(string)] = d
		}
	}
	return out
}

func TestTenantsAreCreatedWithTheirCertificates(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet,
		"-s", "templates/tenants.yaml", "-s", "templates/agent/certificate-clients.yaml",
		"--set", "agent.tenantCertificates.enabled=true",
		"--set", "tenants.platform.persistence.allowed=true",
		"--set", "tenants.platform.persistence.writeQuota=256Mi",
		"--set-string", "tenants.platform.quota.cpu=50%",
		"--set-string", "tenants.platform.quota.memory=8Gi")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	all := docs(t, out)
	tenants := ofKind(all, "Tenant")
	if len(tenants) != 2 || tenants["default"] == nil || tenants["platform"] == nil {
		t.Fatalf("the default tenant always exists, plus the listed ones: %v", tenants)
	}
	spec := tenants["platform"]["spec"].(map[string]any)
	quota := spec["quota"].(map[string]any)
	pers := spec["persistence"].(map[string]any)
	if quota["cpu"] != "50%" || quota["memory"] != "8Gi" || pers["allowed"] != true || pers["writeQuota"] != "256Mi" {
		t.Fatalf("spec %v", spec)
	}
	if dp := tenants["default"]["spec"].(map[string]any)["persistence"].(map[string]any); dp["allowed"] != true {
		t.Fatalf("the default tenant may use persistence unless told otherwise: %v", dp)
	}
	if tenants["default"]["spec"].(map[string]any)["quota"] != nil {
		t.Fatal("the default tenant has no quota")
	}
	certs := ofKind(all, "Certificate")
	for _, name := range []string{"default", "platform"} {
		c := certs["laboratory-agent-client-"+name]
		if c == nil {
			t.Fatalf("no certificate for tenant %s: %v", name, certs)
		}
		cs := c["spec"].(map[string]any)
		if cs["commonName"] != name || cs["secretName"] != "laboratory-agent-client-"+name+"-tls" {
			t.Errorf("tenant %s certificate: %v", name, cs)
		}
	}
}

func TestTenantListingDefaultOverridesItsPolicy(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/tenants.yaml", "--set", "tenants.default.persistence.allowed=false")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	d := ofKind(docs(t, out), "Tenant")["default"]["spec"].(map[string]any)["persistence"].(map[string]any)
	if d["allowed"] == true {
		t.Fatalf("%v", d)
	}
}

func TestTenantNameMustBeADNSLabel(t *testing.T) {
	for _, bad := range []string{"Platform", "a_b", "-x"} {
		out, err := helmTemplate(t, "-s", "templates/tenants.yaml", "--set", "tenants."+bad+".quota.cpu=1")
		if err == nil || !strings.Contains(out, "DNS-1123") {
			t.Errorf("%q must be refused: %v %s", bad, err, out)
		}
	}
}

func TestAgentHasNoCNAllowlistAnymore(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "AGENT_ALLOWED_CLIENT_CNS") {
		t.Fatal("the tenants are the allowlist")
	}
	if !strings.Contains(out, "AGENT_LAB_TOLERATIONS") || !strings.Contains(out, "AGENT_LAB_NODE_SELECTOR") {
		t.Fatal("the agent needs the lab nodes to resolve percentage quotas")
	}
}

// The cert-manager certificate per tenant is a manual fallback, off by default.
func TestTenantCertificatesAreOffByDefault(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/certificate-clients.yaml")...)
	if err == nil && strings.Contains(out, "kind: Certificate") {
		t.Fatalf("a certificate was rendered by default:\n%s", out)
	}
}

func TestEnrollmentWiring(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml", "-s", "templates/operator/configmap.yaml",
		"--set", "agent.enrollment.tokenTTL=2h", "--set", "agent.enrollment.certificateTTL=48h")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"AGENT_MTLS_CLIENT_CA_KEY", "/ca/tls.key", `value: "48h"`, `TENANT_ENROLLMENT_TTL: "2h"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestTenantsNamespaceAndAccess(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/tenants-namespace.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	all := docs(t, out)
	if ofKind(all, "Namespace")["laboratory-tenants"] == nil {
		t.Fatal("the tenants namespace")
	}
	roles := ofKind(all, "Role")
	agent, proxy := roles["laboratory-agent-tenants"], roles["laboratory-proxy-tenants"]
	if agent == nil || proxy == nil {
		t.Fatalf("roles: %v", roles)
	}
	verbs := func(r map[string]any) string {
		v, _ := yaml.Marshal(r["rules"])
		return string(v)
	}
	if strings.Contains(verbs(proxy), "create") || strings.Contains(verbs(proxy), "update") || !strings.Contains(verbs(proxy), "watch") {
		t.Errorf("the proxy only reads: %s", verbs(proxy))
	}
	if !strings.Contains(verbs(agent), "create") || strings.Contains(verbs(agent), "delete") {
		t.Errorf("the agent writes keys, never deletes the Secret: %s", verbs(agent))
	}
}

func TestProxyHasNoSharedLabAccessKey(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, gone := range []string{"LAB_ACCESS_PUBLIC_KEY_PATH", "lab-access-public-key", "/etc/proxy/lab-access"} {
		if strings.Contains(out, gone) {
			t.Errorf("the shared key path is gone, found %q", gone)
		}
	}
}

// The group pods' resources reach the operator and the agent from one chart value.
func TestGroupPodResourcesReachOperatorAndAgent(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/operator/configmap.yaml", "-s", "templates/agent/deployment.yaml",
		"--set", "vpn.resources.cpu=200m", "--set", "vpn.resources.memory=128Mi",
		"--set", "inetGateway.resources.cpu=75m", "--set", "inetGateway.resources.memory=48Mi")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`VPN_CPU: "200m"`, `VPN_MEMORY: "128Mi"`, `GATEWAY_CPU: "75m"`, `GATEWAY_MEMORY: "48Mi"`, `value: "200m"`, `value: "128Mi"`, `value: "75m"`, `value: "48Mi"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// The defaults are the measured ones: a VPN pod of a team of ten needs memory headroom (155Mi was seen with five
// active peers), a gateway is tiny. Requests equal limits (Guaranteed pods).
func TestGroupPodDefaultsAreTheMeasuredOnes(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/operator/configmap.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`VPN_CPU: "100m"`, `VPN_MEMORY: "320Mi"`, `GATEWAY_CPU: "10m"`, `GATEWAY_MEMORY: "32Mi"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestSupportEmailIsRequired(t *testing.T) {
	out, err := helmTemplate(t, "--set", "operator.supportEmail=", "-s", "templates/operator/configmap.yaml")
	if err == nil || !strings.Contains(out, "supportEmail") {
		t.Fatalf("an empty operator.supportEmail must fail the render: %v\n%s", err, out)
	}
}

// The ACME directory comes from values: a preset chosen by staging, or the explicit server.
func TestACMEServerFromValues(t *testing.T) {
	acme := func(extra ...string) string {
		out, err := helmTemplate(t, append([]string{"-s", "templates/certmanager/clusterissuer.yaml",
			"--set", "certManager.selfSigned=false", "--set", "certManager.email=a@example.com",
			"--set", "certManager.dns01.cloudflare.apiTokenSecretRef.name=cf"}, extra...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	if out := acme(); !strings.Contains(out, "https://acme-staging-v02.api.letsencrypt.org/directory") {
		t.Errorf("staging preset:\n%s", out)
	}
	if out := acme("--set", "certManager.staging=false"); !strings.Contains(out, "https://acme-v02.api.letsencrypt.org/directory") {
		t.Errorf("production preset:\n%s", out)
	}
	if out := acme("--set", "certManager.acme.server=https://acme.example.org/dir"); !strings.Contains(out, "https://acme.example.org/dir") || strings.Contains(out, "acme-v02") || strings.Contains(out, "acme-staging") {
		t.Errorf("explicit server:\n%s", out)
	}
}

func TestAgentGetsTheCachePinTTLFromValues(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "registry.cache.enabled=true", "--set", "registry.cache.pinTTL=45m", "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "AGENT_CACHE_PIN_TTL") || !strings.Contains(out, `"45m"`) {
		t.Errorf("pin ttl:\n%s", out)
	}
}

// Every tunable is passed explicitly, with the value that the code default mirrors.
func TestRemainingTunablesArePassedWithTheirDefaults(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "devices.statePersistence.enabled=true",
		"-s", "templates/operator/configmap.yaml", "-s", "templates/agent/deployment.yaml", "-s", "templates/node-agent/daemonset.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`NETWORK_POLICY_ENABLED: "true"`, `VPN_STATS_INTERVAL: "30s"`, `STATE_RETENTION_INTERVAL: "10m"`,
		"name: AGENT_ID\n              value: \"laboratory-agent\"", "name: AGENT_TENANT_STATUS_INTERVAL\n              value: \"30s\"",
		"name: OVS_BRIDGE\n              value: \"br-ovs\""} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
}

func TestRemainingTunablesFollowValues(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "devices.statePersistence.enabled=true",
		"--set", "operator.groupNetworkPolicy.enabled=false", "--set", "vpn.statsInterval=1m", "--set", "devices.statePersistence.retentionInterval=1h",
		"--set", "agent.id=ctl-1", "--set", "agent.tenantStatusInterval=5s", "--set", "nodeAgent.ovsBridge=br-x",
		"-s", "templates/operator/configmap.yaml", "-s", "templates/agent/deployment.yaml", "-s", "templates/node-agent/daemonset.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`NETWORK_POLICY_ENABLED: "false"`, `VPN_STATS_INTERVAL: "1m"`, `STATE_RETENTION_INTERVAL: "1h"`,
		"name: AGENT_ID\n              value: \"ctl-1\"", "name: AGENT_TENANT_STATUS_INTERVAL\n              value: \"5s\"",
		"name: OVS_BRIDGE\n              value: \"br-x\""} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// GetFeatures reports the cluster values the operator uses: the agent gets the same ones from the same keys.
func TestAgentGetsTheFeatureValues(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "devices.statePersistence.debounce=9s", "--set", "devices.statePersistence.writeQuota=1Gi",
		"--set", "scheduler.maxPods=7", "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"name: AGENT_STATE_DEBOUNCE\n              value: \"9s\"", "name: AGENT_STATE_WRITE_QUOTA\n              value: \"1Gi\"",
		"name: AGENT_STATE_MAX_FILE_SIZE\n              value: \"256Mi\"", "name: AGENT_STATE_EXCLUDE_PATHS\n              value: \"/tmp,/var/tmp,/run\"",
		"name: AGENT_SCHEDULER_MAX_PODS\n              value: \"7\"", "name: AGENT_SCHEDULER_ENABLED\n              value: \"true\"",
		"name: AGENT_PROXY_ACCESS_TOKEN_MAX_TTL\n              value: \"5m\"", "name: AGENT_PROXY_SESSION_MAX_TTL\n              value: \"24h\"",
		"name: AGENT_BASE_DOMAIN", "name: AGENT_PUBLIC_VPN_ENDPOINT"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestLimitsReachTheAgentAndTheOperator(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml", "-s", "templates/operator/configmap.yaml",
		"--set", "limits.tenant.maxLabs=5", "--set", "limits.lab.maxDevices=4")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"name: AGENT_LIMIT_DEVICE_MAX_CPU\n              value: \"500m\"", "name: AGENT_LIMIT_DEVICE_MAX_MEMORY\n              value: \"512Mi\"",
		"name: AGENT_LIMIT_DEVICE_DEFAULT_CPU\n              value: \"100m\"", "name: AGENT_LIMIT_DEVICE_DEFAULT_MEMORY\n              value: \"256Mi\"",
		"name: AGENT_LIMIT_LAB_MAX_DEVICES\n              value: \"4\"", "name: AGENT_LIMIT_LAB_MAX_CPU\n              value: \"2\"",
		"name: AGENT_LIMIT_LAB_MAX_MEMORY\n              value: \"2Gi\"", "name: AGENT_LIMIT_TENANT_MAX_LABS\n              value: \"5\"",
		`DEVICE_DEFAULT_CPU: "100m"`, `DEVICE_DEFAULT_MEMORY: "256Mi"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestOldDeviceDefaultsKeyIsRefused(t *testing.T) {
	out, err := helmTemplate(t, "--set", "scheduler.deviceDefaults.cpu=1")
	if err == nil || !strings.Contains(out, "limits.device") {
		t.Fatalf("scheduler.deviceDefaults must be refused: %v\n%s", err, out)
	}
}
