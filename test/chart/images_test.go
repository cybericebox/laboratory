package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// A tenant's image policy and registry credentials are part of its spec, and the platform's
// deny list and the cache's node address reach the agent.
func TestTenantImagePolicyReachesTheTenantAndTheAgent(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet,
		"-s", "templates/tenants.yaml", "-s", "templates/agent/deployment.yaml",
		"--set", "registry.enabled=true", "--set", "registry.cache.enabled=true",
		"--set", "tenants.acme.images.pullSecret=acme-registry",
		"--set", "tenants.acme.images.allow={ghcr.io/acme/,docker.io/library}",
		"--set", "images.tenantDeny={ghcr.io/platform/,quay.io/platform}")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	all := docs(t, out)
	spec := ofKind(all, "Tenant")["acme"]["spec"].(map[string]any)
	img := spec["images"].(map[string]any)
	allow := img["allow"].([]any)
	if img["pullSecret"] != "acme-registry" || len(allow) != 2 || allow[0] != "ghcr.io/acme/" {
		t.Fatalf("tenant images: %v", img)
	}
	var dep appsv1.Deployment
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.Contains(d, "kind: Deployment") {
			if err := yaml.Unmarshal([]byte(d), &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["AGENT_IMAGE_DENY"] != "ghcr.io/platform/,quay.io/platform" {
		t.Errorf("deny list: %q", env["AGENT_IMAGE_DENY"])
	}
	// The node prefix of the cache (localhost:5035) is derived in the image (names.RegistryNodePrefix), not passed.
	if _, set := env["IMAGE_CACHE_PREFIX"]; set {
		t.Errorf("the cache node prefix is derived, not passed: %q", env["IMAGE_CACHE_PREFIX"])
	}
}

// With no deny list the agent gets no AGENT_IMAGE_DENY.
func TestNoDenyListNoEnv(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "AGENT_IMAGE_DENY") {
		t.Error("no deny list, no variable")
	}
}
