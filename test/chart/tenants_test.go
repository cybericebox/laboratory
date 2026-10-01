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
