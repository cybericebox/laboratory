package chart_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// podSpecs returns the pod spec of every workload of a rendered chart, keyed by "<kind>/<name>".
func podSpecs(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	res := map[string]map[string]any{}
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "Deployment" && kind != "DaemonSet" && kind != "StatefulSet" {
			continue
		}
		md := d["metadata"].(map[string]any)
		spec := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		res[kind+"/"+md["name"].(string)] = spec
	}
	return res
}

func containersOf(spec map[string]any, key string) []map[string]any {
	var out []map[string]any
	list, _ := spec[key].([]any)
	for _, c := range list {
		out = append(out, c.(map[string]any))
	}
	return out
}

func fullChart(t *testing.T, extra ...string) string {
	t.Helper()
	set := append(append([]string{}, agentSet...), "--set", "devices.statePersistence.enabled=true", "--set", "registry.cache.enabled=true")
	out, err := helmTemplate(t, append(set, extra...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

// Every service pod of the laboratory, init and sidecar containers included, has requests and limits for CPU and memory.
func TestEveryServicePodHasRequestsAndLimits(t *testing.T) {
	specs := podSpecs(t, fullChart(t))
	if len(specs) < 5 {
		t.Fatalf("expected the operator, agent, proxy, registry and node-agent: %v", len(specs))
	}
	for name, spec := range specs {
		for _, key := range []string{"initContainers", "containers"} {
			for _, c := range containersOf(spec, key) {
				res, _ := c["resources"].(map[string]any)
				for _, kind := range []string{"requests", "limits"} {
					m, _ := res[kind].(map[string]any)
					for _, r := range []string{"cpu", "memory"} {
						if m[r] == nil {
							t.Errorf("%s %s %s: no %s %s", name, key, c["name"], kind, r)
						}
					}
				}
			}
		}
	}
}

func securityOf(c map[string]any) map[string]any {
	sc, _ := c["securityContext"].(map[string]any)
	return sc
}

func dropsAll(sc map[string]any) bool {
	caps, _ := sc["capabilities"].(map[string]any)
	drop, _ := caps["drop"].([]any)
	return len(drop) == 1 && drop[0] == "ALL"
}

// The agent and the proxy run non-root with every capability dropped, a read-only root and the runtime's seccomp profile;
// zot runs non-root with seccomp.
func TestServicePodsAreHardened(t *testing.T) {
	specs := podSpecs(t, fullChart(t))
	podLevel := func(name string) map[string]any {
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("no %s in %v", name, specs)
		}
		sc, _ := spec["securityContext"].(map[string]any)
		if sc["runAsNonRoot"] != true {
			t.Errorf("%s must run as non-root: %v", name, sc)
		}
		if sp, _ := sc["seccompProfile"].(map[string]any); sp["type"] != "RuntimeDefault" {
			t.Errorf("%s seccomp: %v", name, sc)
		}
		return spec
	}
	for _, name := range []string{"Deployment/laboratory-agent", "Deployment/laboratory-proxy"} {
		for _, c := range containersOf(podLevel(name), "containers") {
			sc := securityOf(c)
			if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false || !dropsAll(sc) {
				t.Errorf("%s %s: %v", name, c["name"], sc)
			}
		}
	}
	podLevel("Deployment/laboratory-registry")
	// the node-agent drives OVS and pod networking and stays privileged
	if _, ok := specs["DaemonSet/laboratory-node-agent"]; !ok {
		t.Fatal("no node-agent")
	}
}

// zot: the snapshots of the labs and the base repository are not anonymous; the public image cache is.
func TestRegistryKeepsSnapshotsPrivate(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "devices.statePersistence.enabled=true", "--set", "registry.cache.enabled=true",
		"-s", "templates/registry/configmap.yaml", "-s", "templates/registry/secret.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	var cfg struct {
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
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.Contains(d, "kind: ConfigMap") {
			if err := yaml.Unmarshal([]byte(d), &cm); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := json.Unmarshal([]byte(cm.Data["config.json"]), &cfg); err != nil {
		t.Fatalf("%v\n%s", err, cm.Data["config.json"])
	}
	repos := cfg.HTTP.AccessControl.Repositories
	if _, catchAll := repos["**"]; catchAll {
		t.Error("there must be no catch-all pattern")
	}
	for _, private := range []string{"lab/**", "base", "base/**"} {
		r, ok := repos[private]
		if !ok || len(r.AnonymousPolicy) != 0 {
			t.Errorf("%s must exist and not be anonymous: %+v", private, r)
			continue
		}
		var readers, writers bool
		for _, p := range r.Policies {
			readers = readers || (strings.Join(p.Users, ",") == "reader" && strings.Join(p.Actions, ",") == "read")
			writers = writers || strings.Join(p.Users, ",") == "writer"
		}
		if !readers || !writers {
			t.Errorf("%s: the reader reads, the writer writes: %+v", private, r.Policies)
		}
	}
	for _, public := range []string{"docker.io/**", "ghcr.io/**", "quay.io/**", "registry.k8s.io/**"} {
		r, ok := repos[public]
		if !ok || len(r.AnonymousPolicy) != 1 || r.AnonymousPolicy[0] != "read" {
			t.Errorf("%s is the public cache: anonymous read: %+v", public, r)
			continue
		}
		// the writer account is on every node: it may read the cache and never write or delete there (zot's own sync fills it)
		for _, p := range r.Policies {
			if strings.Join(p.Users, ",") == "writer" && strings.Join(p.Actions, ",") != "read" {
				t.Errorf("%s: the writer must only read the cache: %v", public, p.Actions)
			}
		}
	}
	if strings.Contains(cm.Data["config.json"], "adminPolicy") {
		t.Error("no account may have rights over every repository")
	}
	// both accounts exist, and the agent gets the reader in its own namespace
	if !strings.Contains(out, "readerPassword:") || !strings.Contains(out, "reader:$2") || !strings.Contains(out, "writer:$2") {
		t.Errorf("the registry secret needs a reader and a writer account:\n%s", out)
	}
	agentOut, err := helmTemplate(t, append(agentSet, "--set", "devices.statePersistence.enabled=true", "-s", "templates/registry/secret.yaml", "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: laboratory-registry-reader", "namespace: laboratory-agent", "name: AGENT_REGISTRY_USER", "name: AGENT_REGISTRY_PASSWORD"} {
		if !strings.Contains(agentOut, want) {
			t.Errorf("missing %q", want)
		}
	}
	// the reader reaches the node-agent too (its forwarder adds it to the pulls of snapshots)
	nodeOut, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/node-agent/daemonset.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nodeOut, "STATE_REGISTRY_READER_PASSWORD") || !strings.Contains(nodeOut, "key: readerPassword") {
		t.Error("the node-agent needs the reader account")
	}
}

// The node-agent is the CNI: a device-plugin directory that does not exist must not keep its pod in Init (and with it every new
// pod of the node). The directory is the kubelet's fixed one, and the plugin looks for the tun device in the host's sysfs.
func TestDevicePluginCannotBlockTheCNI(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/node-agent/daemonset.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var found int
	for _, d := range docs(t, out) {
		if d["kind"] != "DaemonSet" {
			continue
		}
		spec := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		for _, v := range spec["volumes"].([]any) {
			vol := v.(map[string]any)
			hp, _ := vol["hostPath"].(map[string]any)
			switch vol["name"] {
			case "device-plugins":
				found++
				if hp["path"] != "/var/lib/kubelet/device-plugins" {
					t.Errorf("the kubelet's device-plugins directory is fixed: %v", hp["path"])
				}
				if hp["type"] != "DirectoryOrCreate" {
					t.Errorf("a missing directory must not block the pod: type %v", hp["type"])
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("the device-plugin volume: found %d of 1", found)
	}
	if !regexp.MustCompile(`TUN_CHECK_PATH\s+value: /sys/class/misc/tun/dev`).MatchString(out) {
		t.Error("the plugin must check the host's sysfs entry of tun, not the container's /dev")
	}
}

// zot is not root: its data directory is handed to it by one root init container that does nothing else.
func TestZotDataDirectoryIsOwnedByAnInitContainer(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/registry/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	spec := podSpecs(t, out)["Deployment/laboratory-registry"]
	inits := containersOf(spec, "initContainers")
	if len(inits) != 1 || inits[0]["name"] != "own-data" {
		t.Fatalf("init containers: %v", inits)
	}
	sc := securityOf(inits[0])
	if sc["runAsUser"] != float64(0) || sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true || !dropsAll(sc) {
		t.Errorf("own-data security: %v", sc)
	}
	cmd, _ := inits[0]["command"].([]any)
	if len(cmd) != 3 || cmd[2] != "chown -R 65532:65532 /var/lib/registry" {
		t.Errorf("it does one thing: %v", cmd)
	}
	for _, c := range containersOf(spec, "containers") {
		if c["name"] == "zot" && securityOf(c)["runAsUser"] == float64(0) {
			t.Error("zot itself must not run as root")
		}
	}
}

// The error journal needs no log access: the components write Events (the proxy gets that in a Role of the release namespace), the agent reads those
// of the release namespace with a read-only Role, and each component knows where to publish.
func TestErrorJournalPermissionsAndWiring(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/role-events.yaml", "-s", "templates/proxy/role-events.yaml", "-s", "templates/proxy/clusterrole.yaml",
		"-s", "templates/agent/deployment.yaml", "-s", "templates/proxy/deployment.yaml", "-s", "templates/node-agent/daemonset.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var agentRole, proxyRole map[string]any
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		switch md["name"] {
		case "laboratory-agent-events":
			if d["kind"] == "Role" {
				agentRole = d
			}
		case "laboratory-proxy-events":
			if d["kind"] == "Role" {
				proxyRole = d
				if md["namespace"] != "laboratory" && md["namespace"] == nil {
					t.Error("the proxy events Role is namespaced")
				}
			}
		case "laboratory-proxy":
			if d["kind"] == "ClusterRole" {
				for _, r := range rulesOf(t, d) {
					if has(r.Resources, "events") {
						t.Errorf("the proxy ClusterRole must not grant events: %+v", r)
					}
				}
			}
		}
	}
	if agentRole == nil || proxyRole == nil {
		t.Fatalf("roles: %v %v", agentRole != nil, proxyRole != nil)
	}
	for _, r := range rulesOf(t, agentRole) {
		if !has(r.Resources, "events") || has(r.Verbs, "create") || has(r.Verbs, "update") || has(r.Verbs, "delete") || !has(r.Verbs, "list") {
			t.Errorf("the agent only reads events: %+v", r)
		}
	}
	creates := false
	for _, r := range rulesOf(t, proxyRole) {
		creates = creates || (has(r.Resources, "events") && has(r.Verbs, "create") && has(r.Verbs, "patch"))
	}
	if !creates {
		t.Error("the proxy publishes its errors as events")
	}
	for _, want := range []string{"name: AGENT_RELEASE_NAMESPACE", "name: ERROR_JOURNAL_NAMESPACE"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(out, "name: ERROR_JOURNAL_NAMESPACE"); n != 3 {
		t.Errorf("l7, demux and the node-agent each know where to publish: %d", n)
	}
}

// The agent runs two replicas by default, spread over nodes, with a budget; and mTLS off needs the insecure flag by name.
func TestAgentReplicasAndInsecureFlag(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml", "-s", "templates/agent/pdb.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "replicas: 2") || !strings.Contains(out, "kind: PodDisruptionBudget") || !strings.Contains(out, "topologySpreadConstraints") {
		t.Errorf("the agent must default to two replicas with a budget and a spread:\n%s", out)
	}
	for _, want := range []string{"AGENT_ENROLL_MAX_MESSAGE_BYTES", "AGENT_MAX_CONCURRENT_STREAMS", "AGENT_STREAM_RECHECK", "AGENT_ALLOW_INSECURE"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	// one replica: no budget (it would block drains)
	out, err = helmTemplate(t, append(agentSet, "--set", "agent.replicas=1", "-s", "templates/agent/pdb.yaml")...)
	if err == nil && strings.Contains(out, "PodDisruptionBudget") {
		t.Error("a budget on a single replica would block every drain")
	}
	// mTLS off without the flag fails the render
	if _, err := helmTemplate(t, append(agentSet, "--set", "agent.mtls.enabled=false", "-s", "templates/agent/deployment.yaml")...); err == nil {
		t.Error("mtls off must need agent.allowInsecure")
	}
	if _, err := helmTemplate(t, append(agentSet, "--set", "agent.mtls.enabled=false", "--set", "agent.allowInsecure=true", "-s", "templates/agent/deployment.yaml")...); err != nil {
		t.Errorf("mtls off with the flag: %v", err)
	}
}

// R-8: without ValidatingAdmissionPolicy the chart does not install silently; the opt-out is explicit.
func TestAdmissionPolicyIsNotSkippedSilently(t *testing.T) {
	if out, err := helmTemplate(t, "--kube-version", "1.29.0", "-s", "templates/operator/admission-policy.yaml"); err == nil {
		t.Fatalf("a cluster older than 1.30 must fail the render:\n%s", out)
	} else if !strings.Contains(err.Error()+out, "admissionPolicy") {
		t.Errorf("the failure must say what to do: %v %s", err, out)
	}
	out, err := helmTemplate(t, "--kube-version", "1.29.0", "--set", "operator.admissionPolicy.enabled=false", "-s", "templates/operator/configmap.yaml")
	if err != nil {
		t.Fatalf("the opt-out must render: %v\n%s", err, out)
	}
	if !strings.Contains(out, `OPERATOR_REQUIRE_ADMISSION_POLICY: "false"`) {
		t.Errorf("an opted-out chart must not make the operator require the policies:\n%s", out)
	}
	out, err = helmTemplate(t, "-s", "templates/operator/configmap.yaml")
	if err != nil || !strings.Contains(out, `OPERATOR_REQUIRE_ADMISSION_POLICY: "true"`) {
		t.Errorf("by default the operator checks the policies at start: %v\n%s", err, out)
	}
}

// L-14: the namespaces of the agent and the proxy admit only restricted pods, and the pods the chart puts there are restricted ones.
func TestAgentAndProxyNamespacesEnforcePodSecurity(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/namespace.yaml", "-s", "templates/proxy/namespace.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	n := 0
	for name, ns := range ofKind(docs(t, out), "Namespace") {
		labels, _ := ns["metadata"].(map[string]any)["labels"].(map[string]any)
		if labels["pod-security.kubernetes.io/enforce"] != "restricted" {
			t.Errorf("namespace %s must enforce restricted: %v", name, labels)
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d namespaces rendered", n)
	}
}

// L-10: zot has no ServiceAccount token (it never talks to the API) and its writer account cannot touch the public cache.
func TestRegistryPodHasNoServiceAccountToken(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/registry/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "automountServiceAccountToken: false") {
		t.Errorf("zot must not mount a token:\n%s", out)
	}
}
