package chart_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

var platformRegistryTemplates = []string{
	"-s", "templates/registry/configmap.yaml", "-s", "templates/registry/secret.yaml", "-s", "templates/netpol/registry.yaml",
}

// zotConfig is the access control and the retention of the registry's config.json, as rendered.
type zotConfig struct {
	Storage struct {
		Retention struct {
			Policies []struct {
				Repositories []string `json:"repositories"`
			} `json:"policies"`
		} `json:"retention"`
	} `json:"storage"`
	HTTP struct {
		AccessControl struct {
			Repositories map[string]struct {
				AnonymousPolicy []string `json:"anonymousPolicy"`
				Policies        []struct {
					Users   []string `json:"users"`
					Actions []string `json:"actions"`
				} `json:"policies"`
			} `json:"repositories"`
		} `json:"accessControl"`
	} `json:"http"`
}

func renderZot(t *testing.T, extra ...string) (zotConfig, []corev1.Secret, []map[string]any) {
	t.Helper()
	out, err := helmTemplate(t, append(append([]string{}, platformRegistryTemplates...), extra...)...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var cfg zotConfig
	var secrets []corev1.Secret
	var all []map[string]any
	for _, d := range docs(t, out) {
		all = append(all, d)
		switch d["kind"] {
		case "ConfigMap":
			cm := d["data"].(map[string]any)["config.json"].(string)
			if err := json.Unmarshal([]byte(cm), &cfg); err != nil {
				t.Fatalf("zot config: %v\n%s", err, cm)
			}
		case "Secret":
			raw, _ := json.Marshal(d)
			var s corev1.Secret
			if err := yaml.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			secrets = append(secrets, s)
		}
	}
	return cfg, secrets, all
}

func secretIn(secrets []corev1.Secret, namespace, name string) *corev1.Secret {
	for i := range secrets {
		if secrets[i].Namespace == namespace && secrets[i].Name == name {
			return &secrets[i]
		}
	}
	return nil
}

// With registry.platform off (the default) the registry has no platform namespace, no account for it and no Secret: nothing changes.
func TestPlatformRegistryOffRendersNothing(t *testing.T) {
	cfg, secrets, _ := renderZot(t, agentSet...)
	if _, ok := cfg.HTTP.AccessControl.Repositories["platform/**"]; ok {
		t.Error("platform/** is a repository pattern although registry.platform is off")
	}
	for _, s := range secrets {
		if s.Name == "laboratory-registry-platform" {
			t.Errorf("the Secret of the platform account exists with the feature off (namespace %s)", s.Namespace)
		}
		if _, ok := s.StringData["platformPassword"]; ok {
			t.Error("the registry Secret carries a platform password with the feature off")
		}
		if strings.Contains(s.StringData["htpasswd"], "platform-writer") {
			t.Error("the registry knows the account platform-writer with the feature off")
		}
	}
}

func TestPlatformRegistryOnAddsThePolicyTheAccountAndTheSecrets(t *testing.T) {
	cfg, secrets, all := renderZot(t, append(agentSet,
		"--set", "registry.platform.enabled=true",
		"--set", "registry.platform.writerNamespaces={platform,tools}",
		"--set", "registry.cache.enabled=true")...)

	// zot: platform/** is private; the reader reads, platform-writer has every right, nobody else is listed
	repo, ok := cfg.HTTP.AccessControl.Repositories["platform/**"]
	if !ok {
		t.Fatal("platform/** is missing from the access control")
	}
	if len(repo.AnonymousPolicy) != 0 {
		t.Errorf("platform/** must not be anonymous: %v", repo.AnonymousPolicy)
	}
	rights := map[string][]string{}
	for _, p := range repo.Policies {
		for _, u := range p.Users {
			rights[u] = p.Actions
		}
	}
	if len(rights) != 2 || strings.Join(rights["reader"], ",") != "read" || strings.Join(rights["platform-writer"], ",") != "read,create,update,delete" {
		t.Errorf("rights in platform/**: %v", rights)
	}
	if _, ok := rights["writer"]; ok {
		t.Error("the snapshot writer, which sits on every node, must have no right in platform/**")
	}
	// platform-writer is listed nowhere else, and the snapshot patterns did not change
	for name, r := range cfg.HTTP.AccessControl.Repositories {
		for _, p := range r.Policies {
			for _, u := range p.Users {
				if u == "platform-writer" && name != "platform/**" {
					t.Errorf("platform-writer has a right in %s", name)
				}
			}
		}
	}
	for _, name := range []string{"lab/**", "base", "base/**"} {
		for _, p := range cfg.HTTP.AccessControl.Repositories[name].Policies {
			for _, u := range p.Users {
				if u == "reader" && len(p.Actions) != 1 {
					t.Errorf("the reader in %s: %v", name, p.Actions)
				}
			}
		}
	}
	// the cache retention names only the cache's own globs
	if len(cfg.Storage.Retention.Policies) == 0 {
		t.Fatal("the cache is on and has a retention policy")
	}
	for _, pol := range cfg.Storage.Retention.Policies {
		for _, g := range pol.Repositories {
			if g == "platform/**" || strings.HasPrefix(g, "platform") || g == "**" {
				t.Errorf("a retention policy of the cache reaches platform/**: %q", g)
			}
		}
	}

	// the accounts: the registry Secret holds the platform password and its htpasswd line; the platform Secrets carry the same password
	reg := secretIn(secrets, "laboratory-system", "laboratory-registry")
	if reg == nil {
		t.Fatal("no registry Secret")
	}
	pw := reg.StringData["platformPassword"]
	if len(pw) != 40 || !strings.Contains(reg.StringData["htpasswd"], "platform-writer:") {
		t.Errorf("registry Secret: password %d characters, htpasswd has the account: %v", len(pw), strings.Contains(reg.StringData["htpasswd"], "platform-writer:"))
	}
	if pw == reg.StringData["password"] || pw == reg.StringData["readerPassword"] {
		t.Error("the platform account shares a password with another account")
	}
	for _, ns := range []string{"laboratory-system", "platform", "tools"} {
		s := secretIn(secrets, ns, "laboratory-registry-platform")
		if s == nil {
			t.Fatalf("no Secret laboratory-registry-platform in %s", ns)
		}
		if s.StringData["username"] != "platform-writer" || s.StringData["password"] != pw || s.StringData["host"] != "laboratory-registry.laboratory-system.svc:5000" {
			t.Errorf("Secret in %s: user %q, same password %v, host %q", ns, s.StringData["username"], s.StringData["password"] == pw, s.StringData["host"])
		}
	}
	if n := countSecrets(secrets, "laboratory-registry-platform"); n != 3 {
		t.Errorf("%d platform Secrets, want one in the release namespace and one per writer namespace", n)
	}

	// the network policy lets the writer namespaces in on port 5000, and only them
	var pol map[string]any
	for _, d := range all {
		if d["kind"] == "CiliumNetworkPolicy" {
			pol = d
		}
	}
	if pol == nil {
		t.Fatal("no network policy")
	}
	raw, _ := yaml.Marshal(pol)
	for _, ns := range []string{"platform", "tools"} {
		if !strings.Contains(string(raw), "k8s:io.kubernetes.pod.namespace: "+ns) {
			t.Errorf("the registry network policy does not let namespace %s in", ns)
		}
	}
}

func countSecrets(secrets []corev1.Secret, name string) int {
	n := 0
	for _, s := range secrets {
		if s.Name == name {
			n++
		}
	}
	return n
}

func TestPlatformRegistryNetworkPolicyCanBeNarrowedToPodLabels(t *testing.T) {
	_, _, all := renderZot(t, append(agentSet,
		"--set", "registry.platform.enabled=true",
		"--set", "registry.platform.writerNamespaces={platform}",
		"--set", "registry.platform.writerPodLabels.app=backend")...)
	for _, d := range all {
		if d["kind"] != "CiliumNetworkPolicy" {
			continue
		}
		raw, _ := yaml.Marshal(d)
		if !strings.Contains(string(raw), "app: backend") {
			t.Errorf("the pod label is not in the policy:\n%s", raw)
		}
		return
	}
	t.Fatal("no network policy")
}

func TestPlatformRegistryWithNoWriterNamespaceLetsNobodyIn(t *testing.T) {
	_, secrets, all := renderZot(t, append(agentSet, "--set", "registry.platform.enabled=true")...)
	if countSecrets(secrets, "laboratory-registry-platform") != 1 {
		t.Error("the Secret is only in the release namespace")
	}
	for _, d := range all {
		if d["kind"] == "CiliumNetworkPolicy" {
			raw, _ := yaml.Marshal(d)
			if strings.Count(string(raw), "k8s:io.kubernetes.pod.namespace") != 1 { // the agent's rule only
				t.Errorf("an unexpected namespace is let in:\n%s", raw)
			}
		}
	}
}

func TestPlatformRegistryRefusesNamesThatBelongToTheRegistry(t *testing.T) {
	for _, name := range []string{"platform", "lab", "base"} {
		out, err := helmTemplate(t, append(agentSet,
			"--set", "registry.cache.enabled=true",
			"--set", "registry.cache.extraRegistries[0].name="+name,
			"--set", "registry.cache.extraRegistries[0].url=https://registry.example.com")...)
		if err == nil || !strings.Contains(out, "cannot be named") {
			t.Errorf("an upstream named %q must be refused: %v\n%s", name, err, out)
		}
	}
	out, err := helmTemplate(t, append(agentSet, "--set", "registry.platform.writerNamespaces={Bad_Name}")...)
	if err == nil || !strings.Contains(out, "is not a namespace name") {
		t.Errorf("an invalid namespace must be refused: %v\n%s", err, out)
	}
}
