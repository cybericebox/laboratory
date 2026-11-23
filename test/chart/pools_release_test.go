package chart_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The checklist of section 5 of docs/laboratory/specs/pools-release.md as render assertions: every row is a test that fails when the thing that
// had to go is back. The rows that read the code (internal, cmd) belong to the work packages that change it and to the reviewer's greps; these
// read the chart, what it renders, and its tests' companions (README.md, DEPLOY.md, hack).

const chartDir = "../../charts/laboratory"

// everything is every optional component of the chart, so that the renders below see all of its pods.
var everything = []string{
	"--set", "agent.enabled=true", "--set", "terminal.enabled=true", "--set", "checks.enabled=true", "--set", statePath + "enabled=true",
	"--set", "consoleCables.enabled=true",
	"--set", "imagePullSecrets[0].name=platform-pull", "--set", "operator.networkPolicy.enabled=true",
}

// noMatch fails with the matching lines when pattern is found under dir.
func noMatch(t *testing.T, what, dir, pattern string) {
	t.Helper()
	if out := grepTree(t, dir, pattern); out != "" {
		t.Errorf("%s: found under %s:\n%s", what, dir, out)
	}
}

// both is the whole chart rendered with every component on, once per proxy mode.
func renderedEverything(t *testing.T) []string {
	t.Helper()
	var outs []string
	for _, mode := range []string{"replicas", "daemonset"} {
		out, err := helmTemplate(t, append([]string{"--set", "proxy.mode=" + mode}, everything...)...)
		if err != nil {
			t.Fatalf("helm template (proxy %s): %v\n%s", mode, err, out)
		}
		outs = append(outs, out)
	}
	return outs
}

// podsOf returns the pod spec of every Deployment and DaemonSet in the rendered output, by kind/namespace/name.
func podsOf(t *testing.T, out string) map[string]corev1.PodSpec {
	t.Helper()
	res := map[string]corev1.PodSpec{}
	for _, d := range docs(t, out) {
		raw, _ := yaml.Marshal(d)
		md, _ := d["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		key := d["kind"].(string) + "/" + ns + "/" + name
		switch d["kind"] {
		case "Deployment":
			var dep appsv1.Deployment
			if err := yaml.Unmarshal(raw, &dep); err != nil {
				t.Fatal(err)
			}
			res[key] = dep.Spec.Template.Spec
		case "DaemonSet":
			var ds appsv1.DaemonSet
			if err := yaml.Unmarshal(raw, &ds); err != nil {
				t.Fatal(err)
			}
			res[key] = ds.Spec.Template.Spec
		}
	}
	return res
}

// Row 1: no registry reader or writer password on a lab node.
func TestChecklist1NoRegistryPasswordOnALabNode(t *testing.T) {
	for _, tpl := range []string{"templates/node-agent/daemonset.yaml", "templates/node-state/daemonset.yaml"} {
		out, err := helmTemplate(t, append(append([]string{}, everything...), "-s", tpl)...)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "secretKeyRef") {
			t.Errorf("%s names a Secret:\n%s", tpl, out)
		}
	}
	noMatch(t, "row 1", chartDir, `readerPassword|STATE_REGISTRY_USER|STATE_REGISTRY_PASSWORD|STATE_REGISTRY_READER`)
}

// Row 2: the Secret of the registry, the one with the server key, is not reachable from a node pod, the operator or the agent: the authority is a ConfigMap.
func TestChecklist2NoRegistrySecretReachableFromANode(t *testing.T) {
	const pattern = `laboratory-registry($|[^-A-Za-z0-9])|laboratory-registry-(tls|reader|platform|identity)`
	for _, dir := range []string{"node-agent", "node-state", "operator", "agent"} {
		noMatch(t, "row 2", filepath.Join(chartDir, "templates", dir), pattern)
	}
}

// Row 3: node-state has no right on Secrets.
func TestChecklist3NodeStateReadsNoSecret(t *testing.T) {
	noMatch(t, "row 3", filepath.Join(chartDir, "templates/node-state/rbac.yaml"), `secrets`)
	out, err := helmTemplate(t, "-s", "templates/node-state/rbac.yaml", "--set", statePath+"enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "ClusterRole" && d["kind"] != "Role" {
			continue
		}
		for _, r := range rulesOf(t, d) {
			if has(r.Resources, "secrets") {
				t.Errorf("a Secret rule: %+v", r)
			}
		}
	}
}

// Row 4: the node-agent has no Secret right and no Role in an images namespace.
func TestChecklist4NodeAgentHasNoSecretRight(t *testing.T) {
	noMatch(t, "row 4", filepath.Join(chartDir, "templates/node-agent"), `laboratory-images`)
	noMatch(t, "row 4", filepath.Join(chartDir, "templates/node-agent/clusterrole.yaml"), `secrets`)
	noMatch(t, "row 4", filepath.Join(chartDir, "templates/node-agent/role-ledger.yaml"), `secrets`)
	if _, err := os.Stat(filepath.Join(chartDir, "templates/images-namespace.yaml")); err == nil {
		t.Error("the images namespace is gone: nothing is copied for a node to read")
	}
	out, err := helmTemplate(t, append(append([]string{}, everything...), "--set", "operator.admissionPolicy.enabled=true")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "ClusterRole" && d["kind"] != "Role" {
			continue
		}
		name := d["metadata"].(map[string]any)["name"].(string)
		if !strings.Contains(name, "node-agent") && !strings.Contains(name, "node-state") {
			continue
		}
		for _, r := range rulesOf(t, d) {
			if has(r.Resources, "secrets") {
				t.Errorf("%s has a Secret rule: %+v", name, r)
			}
		}
	}
}

// Row 5: no pod of the lab pool has an image pull secret, whatever imagePullSecrets says; only the pods of the lab-control pool carry it.
func TestChecklist5NoPullSecretOnThePodsOfTheLabPool(t *testing.T) {
	labPool := map[string]bool{"laboratory-node-agent": true, "laboratory-node-state": true, "laboratory-device-access": true, "laboratory-checker": true,
		"laboratory-proxy-wg": true, "laboratory-proxy": true}
	control := map[string]bool{"laboratory-controller-manager": true, "laboratory-agent": true, "laboratory-scheduler": true, "laboratory-registry": true}
	for _, out := range renderedEverything(t) {
		seenLab, seenControl := 0, 0
		for key, spec := range podsOf(t, out) {
			name := key[strings.LastIndex(key, "/")+1:]
			switch {
			case labPool[name]:
				seenLab++
				if len(spec.ImagePullSecrets) != 0 {
					t.Errorf("%s has image pull secrets: %v", key, spec.ImagePullSecrets)
				}
			case control[name]:
				seenControl++
				if len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "platform-pull" {
					t.Errorf("%s (lab-control pool) keeps the platform's pull secret: %v", key, spec.ImagePullSecrets)
				}
			default:
				t.Errorf("%s: a pod this test does not know", key)
			}
		}
		if seenLab != len(labPool) || seenControl != len(control) {
			t.Errorf("rendered %d lab-pool and %d lab-control pods, want %d and %d", seenLab, seenControl, len(labPool), len(control))
		}
	}
	// the files that mention image pull secrets are the lab-control pods' (and the helper that names them for the operator)
	re := regexp.MustCompile(`imagePullSecrets`)
	var files []string
	_ = filepath.WalkDir(filepath.Join(chartDir, "templates"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if raw, _ := os.ReadFile(path); re.Match(raw) {
				files = append(files, filepath.ToSlash(strings.TrimPrefix(path, filepath.Join(chartDir, "templates")+string(filepath.Separator))))
			}
		}
		return nil
	})
	want := map[string]bool{"operator/deployment.yaml": true, "agent/deployment.yaml": true, "lab-scheduler/scheduler.yaml": true, "registry/deployment.yaml": true,
		"_helpers.tpl": true}
	for _, f := range files {
		if !want[f] {
			t.Errorf("%s mentions imagePullSecrets: only the lab-control pods may", f)
		}
	}
}

// Row 5a: the names of the earlier identity and credential schemes are gone from the chart (the ImagePull CRD is generated from the Go types and
// loses its fields with them, WP-3).
func TestChecklist5aRemovedNames(t *testing.T) {
	noMatch(t, "row 5a", filepath.Join(chartDir, "templates"), `RekeyForCache|PlatformCacheSecret|LabCacheRegistrySecret|cachePullSecrets|lab-identity|laboratory-registry-identity-key|identityPassword|derive\(|splitUser|PlatformTenant`)
}

// Row 5b: every container of a chart-made pod that names the node's registry has pullPolicy Always (none does today: the chart's own images come
// from their registry; the operator builds the pods of labs, and its own test covers them).
func TestChecklist5bAlwaysOnEveryCacheBackedContainer(t *testing.T) {
	for _, out := range renderedEverything(t) {
		for key, spec := range podsOf(t, out) {
			for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
				if strings.Contains(c.Image, "localhost:5035") && c.ImagePullPolicy != corev1.PullAlways {
					t.Errorf("%s/%s names the node's registry and pulls %q", key, c.Name, c.ImagePullPolicy)
				}
			}
		}
	}
}

// Row 6: the gate reads upstream credentials in their own namespace and nowhere else.
func TestChecklist6UpstreamCredentialsAreReadByTheGateOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "templates/registry/rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(regexp.MustCompile(`secrets`).FindAll(raw, -1)); n != 1 {
		t.Errorf("registry/rbac.yaml says secrets %d times, want one namespaced rule", n)
	}
	// no ClusterRole that is bound cluster-wide (ClusterRoleBinding) reads Secrets of every namespace; a ClusterRole bound by RoleBindings only is
	// the operator's working role, which it holds in the namespaces it was bound in
	for _, out := range renderedEverything(t) {
		bound := map[string]bool{}
		for _, d := range docs(t, out) {
			if d["kind"] == "ClusterRoleBinding" {
				ref, _ := d["roleRef"].(map[string]any)
				bound[ref["name"].(string)] = true
			}
		}
		for _, d := range docs(t, out) {
			if d["kind"] != "ClusterRole" || !bound[d["metadata"].(map[string]any)["name"].(string)] {
				continue
			}
			for _, r := range rulesOf(t, d) {
				if has(r.Resources, "secrets") && (has(r.Verbs, "list") || has(r.Verbs, "watch") || has(r.Verbs, "get")) {
					t.Errorf("ClusterRole %v reads Secrets in every namespace: %+v", d["metadata"].(map[string]any)["name"], r)
				}
			}
		}
	}
}

// Row 7: the platform does not write to the registry: no writer namespace, no platform account, no platform repository.
func TestChecklist7PlatformDoesNotWriteToTheRegistry(t *testing.T) {
	noMatch(t, "row 7", chartDir, `writerNamespaces|writerPodLabels|platform-writer|platformPassword|registry\.platform|platform/\*\*`)
	noMatch(t, "row 7 (README)", "../../README.md", `registry\.platform|platform-writer`)
	for _, o := range renderedEverything(t) {
		if strings.Contains(o, "platform-writer") || strings.Contains(o, "laboratory-registry-platform") {
			t.Error("the render has a platform account")
		}
	}
}

// Row 7a: no network path from another namespace to the registry, and the gate has one listener.
func TestChecklist7aOnlyTheOperatorAndTheAgentReachTheRegistry(t *testing.T) {
	noMatch(t, "row 7a", chartDir, `GATE_PLATFORM|5444`)
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "-s", "templates/netpol/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range regexp.MustCompile(`k8s:io\.kubernetes\.pod\.namespace: (\S+)`).FindAllStringSubmatch(out, -1) {
		if ns[1] != "laboratory-agent" {
			t.Errorf("a caller from the namespace %s", ns[1])
		}
	}
}

// Row 8: the proxy has no session secret and no session of its own.
func TestChecklist8NoProxySessionSecret(t *testing.T) {
	noMatch(t, "row 8", chartDir, `SESSION_SECRET|sessionSecret|proxy-session|SESSION_IDLE_TTL|SESSION_RENEW_BEFORE|SESSION_MAX_TTL|ACCESS_TOKEN_MAX_TTL|sessionCookieName`)
	for _, out := range renderedEverything(t) {
		for key, spec := range podsOf(t, out) {
			if !strings.Contains(key, "laboratory-proxy") {
				continue
			}
			for _, c := range spec.Containers {
				for _, e := range c.Env {
					if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
						t.Errorf("%s/%s: %s comes from the Secret %s", key, c.Name, e.Name, e.ValueFrom.SecretKeyRef.Name)
					}
				}
			}
		}
	}
}

// Row 9: no secret is shared between the proxy and the device access service.
func TestChecklist9NoSharedDeviceAccessKey(t *testing.T) {
	noMatch(t, "row 9", chartDir, `laboratory-device-access-auth|laboratory-proxy-device-access-auth|TERMINAL_KEY_FILE|terminal\.sharedSecret|KeyFiles`)
	for _, out := range renderedEverything(t) {
		for _, d := range docs(t, out) {
			if d["kind"] == "Secret" && strings.Contains(d["metadata"].(map[string]any)["name"].(string), "device-access") {
				t.Errorf("a Secret for the device access service: %v", d["metadata"])
			}
		}
	}
}

// Row 10: the Gateway is gone (names, templates, values, the kind demo), and the old values file that still sets it fails loudly.
func TestChecklist10NoGateway(t *testing.T) {
	noMatch(t, "row 10", chartDir, `(?i)gateway\.networking|GatewayClass|TLSRoute|UDPRoute|laboratory-gateway($|[^-]|-[^r])|gatewayClassName|Gateway API|cilium-envoy|Cilium Envoy`)
	noMatch(t, "row 10 (kind demo)", "../../hack", `(?i)gateway\.networking|GatewayClass|TLSRoute|UDPRoute|gatewayClassName|Gateway API|cilium-envoy|Cilium Envoy|gatewayAPI`)
	noMatch(t, "row 10 (docs)", "../../README.md", `(?i)Gateway API|TLSRoute|GatewayClass`)
	noMatch(t, "row 10 (docs)", "../../DEPLOY.md", `(?i)Gateway API|TLSRoute|GatewayClass|UDPRoute|cilium-envoy|Cilium Envoy`)
	for _, gone := range []string{"templates/gateway", "templates/agent/tlsroute.yaml"} {
		if _, err := os.Stat(filepath.Join(chartDir, gone)); err == nil {
			t.Errorf("%s exists", gone)
		}
	}
	var v map[string]any
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if _, has := v["gateway"]; has {
		t.Error("the values key gateway is back")
	}
	for _, set := range [][]string{{"--set", "gateway.enabled=true"}, {"--set", "gateway.extraListeners[0].name=x"}, {"--set", "proxy.sharingKey=k"}, {"--set", "proxy.sharedNamespaces={a}"}} {
		if out, err := helmTemplate(t, set...); err == nil || !strings.Contains(out, "was removed") {
			t.Errorf("%v must be refused with its replacement: %v\n%s", set, err, out)
		}
	}
	for _, out := range renderedEverything(t) {
		for _, d := range docs(t, out) {
			if k, _ := d["kind"].(string); k == "Gateway" || k == "GatewayClass" || k == "TLSRoute" || k == "UDPRoute" || strings.Contains(d["apiVersion"].(string), "gateway.networking") {
				t.Errorf("a %s is rendered", k)
			}
			if md, _ := d["metadata"].(map[string]any); md != nil {
				ann, _ := md["annotations"].(map[string]any)
				for k := range ann {
					if strings.HasPrefix(k, "lbipam.cilium.io/") {
						t.Errorf("%v: %s: the shared address is gone", md["name"], k)
					}
				}
			}
		}
	}
}

// Row 11: no PROXY protocol, in the chart or its tests. The names are written in two halves so that this file does not match its own check.
func TestChecklist11NoProxyProtocol(t *testing.T) {
	pattern := "proxy" + "Protocol|PROXY" + "_PROTOCOL|L7_PROXY" + "_PROTOCOL"
	noMatch(t, "row 11", chartDir, pattern)
	noMatch(t, "row 11", ".", pattern)
	noMatch(t, "row 11 (docs)", "../../DEPLOY.md", pattern)
	noMatch(t, "row 11 (docs)", "../../README.md", pattern)
}

// Row 12: no placement fallback: no labWorkloads, no nodeSelector as a value, no laboratory.placement helper.
func TestChecklist12NoFallbackPlacement(t *testing.T) {
	noMatch(t, "row 12", chartDir, `lab`+`Workloads|laboratory\.placement`)
	noMatch(t, "row 12 (docs)", "../../DEPLOY.md", `lab`+`Workloads`)
	noMatch(t, "row 12 (docs)", "../../README.md", `lab`+`Workloads`)
	noMatch(t, "row 12 (kind stand)", "../../hack", `lab`+`Workloads`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, c := range m {
			if k == "nodeSelector" {
				t.Errorf("values.yaml has a nodeSelector at %s", path)
			}
			walk(path+"."+k, c)
		}
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	walk("", v)
	if got, _ := v["pools"].(map[string]any); len(got) != 2 {
		t.Errorf("pools: %v", got)
	}
}

// Row 13: no pod-to-pod stats pull: no port, no value, no rule.
func TestChecklist13NoPodToPodStats(t *testing.T) {
	noMatch(t, "row 13", chartDir, `9188|stats`+`Port|EGRESS_STATS`+`_PORT|selectGatewayPods`)
	noMatch(t, "row 13 (docs)", "../../DEPLOY.md", `9188|stats`+`Port|EGRESS_STATS`+`_PORT`)
}

// Row 14: no cluster DNS in the entry components. The agent keeps one DNS rule: it calls the gate by its Service name and the upstream registries by
// theirs (PrewarmImages); the operator keeps one for the same reason.
func TestChecklist14NoClusterDNSInTheEntryPolicies(t *testing.T) {
	noMatch(t, "row 14", chartDir, `svc\.cluster\.local`)
	noMatch(t, "row 14", filepath.Join(chartDir, "templates/netpol/proxy-l7.yaml"), `kube-dns`)
	noMatch(t, "row 14", filepath.Join(chartDir, "templates/netpol/wg-demux.yaml"), `k8s-app: kube-dns`)
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "-s", "templates/netpol/agent.yaml", "-s", "templates/netpol/controller.yaml", "--set", "operator.networkPolicy.enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "k8s-app: kube-dns") != 2 {
		t.Errorf("the agent and the operator keep their one DNS rule each, for the gate's Service name and the upstream registries:\n%s", out)
	}
}

// Row 15: rights that must not appear: the lab pool's components cannot create Secrets or ServiceAccount tokens, and only the gate may check tokens.
func TestChecklist15RightsThatMustNotAppear(t *testing.T) {
	for _, out := range renderedEverything(t) {
		for _, d := range docs(t, out) {
			if d["kind"] != "ClusterRole" && d["kind"] != "Role" {
				continue
			}
			name := d["metadata"].(map[string]any)["name"].(string)
			labPool := strings.Contains(name, "node-agent") || strings.Contains(name, "node-state") || strings.Contains(name, "proxy") || strings.Contains(name, "device-access") ||
				strings.Contains(name, "checker") || strings.Contains(name, "wg")
			for _, r := range rulesOf(t, d) {
				if has(r.Resources, "tokenreviews") && name != "laboratory-registry-gate" {
					t.Errorf("%s may check tokens: only the gate does", name)
				}
				if !labPool {
					continue
				}
				for _, res := range r.Resources {
					if (res == "secrets" || res == "serviceaccounts/token" || res == "serviceaccounts") && (has(r.Verbs, "create") || has(r.Verbs, "update") || has(r.Verbs, "patch")) {
						t.Errorf("%s (lab pool) may write %s: %+v", name, res, r)
					}
				}
			}
		}
	}
}

// Row 16: no unauthenticated registry hop: no plain port anywhere in the registry's templates and policies.
func TestChecklist16NoPlainRegistryPort(t *testing.T) {
	noMatch(t, "row 16", filepath.Join(chartDir, "templates/registry"), `containerPort: 5000|port: "5000"|port: 5000|GATE_LISTEN`)
	noMatch(t, "row 16", filepath.Join(chartDir, "templates/netpol/registry.yaml"), `5000`)
	for _, o := range renderedEverything(t) {
		for key, spec := range podsOf(t, o) {
			if !strings.Contains(key, "laboratory-registry") {
				continue
			}
			for _, c := range spec.Containers {
				for _, p := range c.Ports {
					if p.ContainerPort == 5000 {
						t.Errorf("%s/%s listens on 5000", key, c.Name)
					}
				}
				for _, e := range c.Env {
					if e.Name == "GATE_LISTEN" {
						t.Errorf("%s/%s has GATE_LISTEN=%s", key, c.Name, e.Value)
					}
				}
			}
		}
	}
}
