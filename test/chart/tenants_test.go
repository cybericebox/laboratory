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
