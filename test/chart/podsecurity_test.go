package chart_test

import (
	"encoding/json"
	"os"
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
	// zot has one account, the gate: nobody reads anything anonymously and no node, operator, agent or platform account exists
	for _, private := range []string{"lab/**", "base", "base/**", "c/**"} {
		r, ok := repos[private]
		if !ok || len(r.AnonymousPolicy) != 0 {
			t.Errorf("%s must exist and not be anonymous: %+v", private, r)
			continue
		}
		if len(r.Policies) != 1 || strings.Join(r.Policies[0].Users, ",") != "gate" || strings.Join(r.Policies[0].Actions, ",") != "read,create,update,delete" {
			t.Errorf("%s belongs to the gate account alone: %+v", private, r.Policies)
		}
	}
	if len(repos) != 4 {
		t.Errorf("the repositories of zot are lab/**, base, base/** and c/** only (nothing for the platform): %v", repos)
	}
	// the image cache is the gate's tree: nobody reads it anonymously, and the only account with a right there is the gate
	for _, upstream := range []string{"docker.io/**", "ghcr.io/**", "quay.io/**", "registry.k8s.io/**"} {
		if r, ok := repos[upstream]; ok {
			t.Errorf("%s: the old public cache patterns are gone (the cache lives under c/**): %+v", upstream, r)
		}
	}
	if strings.Contains(cm.Data["config.json"], "adminPolicy") {
		t.Error("no account may have rights over every repository")
	}
	// one account, the gate's: no reader and no writer password exists, in the Secret or in another namespace
	if !strings.Contains(out, "gatePassword:") || !strings.Contains(out, "gate:$2") {
		t.Errorf("the registry secret needs the gate account:\n%s", out)
	}
	for _, gone := range []string{"readerPassword", "reader:$2", "writer:$2", "readerUsername", "platformPassword", "username: writer", "laboratory-registry-reader"} {
		if strings.Contains(out, gone) {
			t.Errorf("the registry secret still holds %q:\n%s", gone, out)
		}
	}
	nodeOut, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/node-agent/daemonset.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(nodeOut, "secretKeyRef") || strings.Contains(nodeOut, "STATE_REGISTRY_READER") {
		t.Errorf("the node-agent holds no registry credential:\n%s", nodeOut)
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
	// The check path (the host's sysfs entry of tun, not the container's /dev) is baked into the node-agent, so the chart passes none.
	if strings.Contains(out, "TUN_CHECK_PATH") {
		t.Error("TUN_CHECK_PATH is a constant of the image, not an input")
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
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/role-events.yaml", "-s", "templates/proxy/role-events.yaml", "-s", "templates/proxy/clusterrole.yaml", "-s", "templates/wg/role.yaml",
		"-s", "templates/agent/deployment.yaml", "-s", "templates/proxy/deployment.yaml", "-s", "templates/wg/daemonset.yaml", "-s", "templates/node-agent/daemonset.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var agentRole, proxyRole, wgRole map[string]any
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
		case "laboratory-proxy-wg-events":
			if d["kind"] == "Role" {
				wgRole = d
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
	if agentRole == nil || proxyRole == nil || wgRole == nil {
		t.Fatalf("roles: %v %v %v", agentRole != nil, proxyRole != nil, wgRole != nil)
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
	creates = false
	for _, r := range rulesOf(t, wgRole) {
		creates = creates || (has(r.Resources, "events") && has(r.Verbs, "create") && has(r.Verbs, "patch"))
	}
	if !creates {
		t.Error("the demux publishes its errors as events")
	}
	// The proxy and the node-agent are told the release namespace as ERROR_JOURNAL_NAMESPACE, the agent as AGENT_RELEASE_NAMESPACE.
	if !strings.Contains(out, "name: ERROR_JOURNAL_NAMESPACE") || !strings.Contains(out, "name: AGENT_RELEASE_NAMESPACE") {
		t.Errorf("ERROR_JOURNAL_NAMESPACE and AGENT_RELEASE_NAMESPACE must be passed")
	}
	if n := strings.Count(out, "name: ERROR_JOURNAL_NAMESPACE"); n != 3 {
		t.Errorf("l7, demux and the node-agent each know where to publish: %d", n)
	}
}

// The agent runs ONE replica by default (reservation fit and quota are serialized inside one process); more than one is refused unless
// acknowledged, and then it is spread over nodes with a budget. mTLS off needs the insecure flag by name.
func TestAgentReplicasAndInsecureFlag(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "replicas: 1") {
		t.Errorf("the agent must default to one replica:\n%s", out)
	}
	// an empty template is an error of helm -s: no output means no budget
	if pdb, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/pdb.yaml")...); err == nil && strings.Contains(pdb, "PodDisruptionBudget") {
		t.Errorf("a budget on one replica blocks every drain:\n%s", pdb)
	}
	// more than one replica: refused with the reason, until acknowledged
	if out, err := helmTemplate(t, append(agentSet, "--set", "agent.replicas=2")...); err == nil || !strings.Contains(out, "agent.multiReplicaAck") {
		t.Errorf("agent.replicas=2 without the acknowledgement must be refused and name the value: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, append(agentSet, "--set", "agent.replicas=2", "--set", "agent.multiReplicaAck=true", "-s", "templates/agent/deployment.yaml", "-s", "templates/agent/pdb.yaml")...)
	if err != nil {
		t.Fatalf("acknowledged replicas=2: %v\n%s", err, out)
	}
	if !strings.Contains(out, "replicas: 2") || !strings.Contains(out, "kind: PodDisruptionBudget") || !strings.Contains(out, "topologySpreadConstraints") {
		t.Errorf("two acknowledged replicas must be spread over nodes with a budget:\n%s", out)
	}
	// the agent switched off: the replica rule does not apply
	if _, err := helmTemplate(t, "--set", "agent.replicas=3"); err != nil {
		t.Errorf("agent.replicas is only checked when the agent is enabled: %v", err)
	}
	for _, want := range []string{"AGENT_ENROLL_MAX_MESSAGE_BYTES", "AGENT_MAX_CONCURRENT_STREAMS", "AGENT_STREAM_RECHECK", "AGENT_ALLOW_INSECURE"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	// There is one switch: agent.mtls.enabled is gone (a stale values file fails), agent.allowInsecure turns the client check off.
	if _, err := helmTemplate(t, append(agentSet, "--set", "agent.mtls.enabled=false", "-s", "templates/agent/deployment.yaml")...); err == nil {
		t.Error("agent.mtls.enabled was removed and must be refused")
	}
	out, err = helmTemplate(t, append(agentSet, "--set", "agent.allowInsecure=true", "-s", "templates/agent/deployment.yaml")...)
	if err != nil || !regexp.MustCompile(`name: AGENT_ALLOW_INSECURE\s+value: "true"`).MatchString(out) {
		t.Errorf("allowInsecure: %v\n%s", err, out)
	}
	out, _ = helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml")...)
	for _, gone := range []string{"AGENT_TLS_ENABLED", "AGENT_MTLS_ENABLED", "AGENT_TLS_CERT", "AGENT_MTLS_CLIENT_CA"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s is a constant of the image (TLS is always on, the files are where the chart mounts them)", gone)
		}
	}
}

// C-16: the chart refuses a Kubernetes older than 1.35 (the kubelet's check of pull credentials, KEP-2535; user namespaces for device pods,
// native sidecars, admission policies).
func TestChartRefusesKubernetesBelow135(t *testing.T) {
	for _, v := range []string{"1.29.0", "1.32.9", "1.33.0", "1.34.7"} {
		out, err := helmTemplate(t, "--kube-version", v, "-s", "templates/operator/configmap.yaml")
		if err == nil || !strings.Contains(out, "1.35") {
			t.Errorf("%s must be refused with the required version in the message: %v\n%s", v, err, out)
		}
	}
	if out, err := helmTemplate(t, "--kube-version", "1.35.0", "-s", "templates/operator/configmap.yaml"); err != nil {
		t.Errorf("1.35.0 must render: %v\n%s", err, out)
	}
	data, err := os.ReadFile("../../charts/laboratory/Chart.yaml")
	if err != nil || !strings.Contains(string(data), "kubeVersion: \">=1.35.0-0\"") {
		t.Errorf("Chart.yaml must carry kubeVersion >=1.35.0-0: %v", err)
	}
}

// C-16: with devices.security.userNamespaces the admission policy requires hostUsers=false on device pods.
func TestAdmissionPolicyRequiresHostUsersFalseForDevicePods(t *testing.T) {
	const rule = "device pod only with hostUsers=false"
	out, err := helmTemplate(t, "-s", "templates/operator/admission-policy.yaml")
	if err != nil || !strings.Contains(out, rule) {
		t.Errorf("userNamespaces is on by default: the policy needs the rule: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "devices.security.userNamespaces=false", "-s", "templates/operator/admission-policy.yaml")
	if err != nil || strings.Contains(out, rule) {
		t.Errorf("userNamespaces off: no rule: %v\n%s", err, out)
	}
}

// R-8: the admission policies are required with the node-agent (which is always installed): cni-gate sends the pods outside the lg-*
// namespaces to the real CNI, and only the policies keep a lab pod out of them. The opt-out is refused at render time.
func TestAdmissionPolicyOptOutIsRefused(t *testing.T) {
	out, err := helmTemplate(t, "--set", "operator.admissionPolicy.enabled=false", "-s", "templates/operator/configmap.yaml")
	if err == nil || !strings.Contains(out, "nodeAgent.enabled needs operator.admissionPolicy.enabled=true") {
		t.Fatalf("the opt-out must be refused: %v\n%s", err, out)
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

// The registry pod has its own ServiceAccount, whose token the gate needs for TokenReview and its watches. Kubernetes mounts a token per pod, so zot sees
// it too: the account has no right on Secrets outside laboratory-upstream (rbac tests), and zot runs as another user.
func TestRegistryPodHasTheGateServiceAccountOnly(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/registry/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "serviceAccountName: laboratory-registry-gate") || !strings.Contains(out, "automountServiceAccountToken: true") {
		t.Errorf("the registry pod runs as the gate's ServiceAccount with its token:\n%s", out)
	}
}
