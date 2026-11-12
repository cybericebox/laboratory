package chart_test

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

var checksOn = append(append([]string{}, agentSet...), "--set", "checks.enabled=true")

func checkerDaemonSet(t *testing.T, extra ...string) appsv1.DaemonSet {
	t.Helper()
	var ds appsv1.DaemonSet
	render(t, "templates/checker/daemonset.yaml", &ds, append(append([]string{}, checksOn...), extra...)...)
	return ds
}

// decodeDocs splits a rendered multi-document file.
func decodeDocs(t *testing.T, template string, extra ...string) []string {
	t.Helper()
	out, err := helmTemplate(t, append(append([]string{}, extra...), "-s", template)...)
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", template, err, out)
	}
	var docs []string
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.Contains(d, "kind:") {
			docs = append(docs, d)
		}
	}
	return docs
}

// With the option off (the default), and with only the terminal option on, nothing of the checker is rendered.
func TestCheckerIsNotRenderedWhileTheOptionIsOff(t *testing.T) {
	for _, set := range [][]string{agentSet, append(append([]string{}, agentSet...), "--set", "terminal.enabled=true")} {
		out, err := helmTemplate(t, set...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		// The operator may bind the checker's secrets role while the checker is off (a bind permission by name grants nothing without the
		// role); that name is the only mention allowed.
		out = strings.ReplaceAll(out, "laboratory-checker-secrets", "")
		for _, bad := range []string{"laboratory-checker", "app: checker", "checker-health"} {
			if strings.Contains(out, bad) {
				t.Errorf("checks.enabled=false, but the chart renders %q", bad)
			}
		}
	}
}

// The checker is a DaemonSet of its own, apart from the node-agent, and holds no more than it needs.
func TestCheckerDaemonSetIsSeparateAndLean(t *testing.T) {
	ds := checkerDaemonSet(t)
	if ds.Name != "laboratory-checker" || ds.Name == nodeAgent(t).Name {
		t.Fatalf("name %q", ds.Name)
	}
	spec := ds.Spec.Template.Spec
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Errorf("the checker must use no host namespace: %v %v %v", spec.HostNetwork, spec.HostPID, spec.HostIPC)
	}
	if spec.ServiceAccountName != "laboratory-checker" || spec.PriorityClassName != "laboratory-platform" {
		t.Errorf("account %q, priority class %q", spec.ServiceAccountName, spec.PriorityClassName)
	}
	if len(spec.InitContainers) != 0 || len(spec.Containers) != 1 {
		t.Fatalf("one container, no init container: %d, %d", len(spec.InitContainers), len(spec.Containers))
	}
	c := spec.Containers[0]
	if strings.Join(c.Command, " ") != "/node checker" || !strings.Contains(c.Image, "laboratory-node") {
		t.Errorf("command %v image %s: the checker is a command of the node image", c.Command, c.Image)
	}
	if len(c.Ports) != 0 {
		t.Errorf("the checker accepts no inbound connection, but declares ports %v", c.Ports)
	}
	for name, probe := range map[string]*corev1.Probe{"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
		if probe == nil || probe.Exec == nil || probe.HTTPGet != nil || probe.TCPSocket != nil {
			t.Errorf("%s probe must be an exec probe (no port is open): %+v", name, probe)
		}
	}
	sc := c.SecurityContext
	if sc == nil || sc.Privileged == nil || *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("security context %+v", sc)
	}
	// Nothing at all: every capability dropped and none added (it used to hold DAC_READ_SEARCH to read the files of containers from the node),
	// a plain user.
	if sc.Capabilities == nil || strings.Join(capNames(sc.Capabilities.Drop), ",") != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Errorf("capabilities %+v: the checker holds none", sc.Capabilities)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 || sc.RunAsGroup == nil || *sc.RunAsGroup != 65532 {
		t.Errorf("the checker must run as a plain user: %+v", sc)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", spec.SecurityContext)
	}
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() || c.Resources.Requests.Memory().IsZero() {
		t.Errorf("the checker has limits of its own: %+v", c.Resources)
	}
	// The one host path: the directory of the checker socket of the device access service, read-only. No runtime socket, no state directory.
	hostPaths := map[string]string{}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			hostPaths[v.Name] = v.HostPath.Path
		}
	}
	if len(hostPaths) != 1 || hostPaths["device-socket"] != "/run/cib-checker" {
		t.Errorf("host paths %v", hostPaths)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "device-socket" && (!m.ReadOnly || m.MountPropagation != nil) {
			t.Errorf("the socket directory must be mounted read-only: %+v", m)
		}
	}
	if raw, _ := yaml.Marshal(spec); strings.Contains(string(raw), "containerd") || strings.Contains(string(raw), "cri-sock") || strings.Contains(string(raw), "DAC_READ_SEARCH") {
		t.Errorf("the checker pod still mentions the runtime or a capability to read files:\n%s", raw)
	}
	env := envOf(c)
	for _, k := range []string{"NODE_NAME", "POD_NAME", "POD_NAMESPACE", "DEVICE_ACCESS_SOCKET", "CHECKER_MAX_CONCURRENT", "CHECKER_MAX_TIME_LIMIT_SECONDS"} {
		if _, ok := env[k]; !ok {
			t.Errorf("env %s missing", k)
		}
	}
	for _, gone := range []string{"CRI_SOCK", "CONTAINERD_STATE", "CONTAINERD_NAMESPACE"} {
		if _, ok := env[gone]; ok {
			t.Errorf("env %s must be gone", gone)
		}
	}
}

func capNames(in []corev1.Capability) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = string(c)
	}
	return out
}

// The values reach the pod: the socket directory, the concurrency, the ceiling and the placement of the lab nodes.
func TestCheckerFollowsItsValuesAndTheLabNodes(t *testing.T) {
	ds := checkerDaemonSet(t, "--set", "checks.socketDir=/var/run/cib", "--set", "checks.maxConcurrent=9", "--set", "checks.maxTimeLimitSeconds=45",
		"--set", "checks.socketUid=2000", "--set", "checks.socketGid=2001", "--set", "labWorkloads.nodeSelector.pool=labs")
	spec := ds.Spec.Template.Spec
	env := envOf(spec.Containers[0])
	if env["DEVICE_ACCESS_SOCKET"].Value != "/var/run/cib/checker.sock" || env["CHECKER_MAX_CONCURRENT"].Value != "9" || env["CHECKER_MAX_TIME_LIMIT_SECONDS"].Value != "45" {
		t.Errorf("env %v", env)
	}
	if sc := spec.Containers[0].SecurityContext; *sc.RunAsUser != 2000 || *sc.RunAsGroup != 2001 {
		t.Errorf("user %v group %v", *sc.RunAsUser, *sc.RunAsGroup)
	}
	if spec.NodeSelector["pool"] != "labs" {
		t.Errorf("node selector %v: the checker runs where the labs run", spec.NodeSelector)
	}
	for _, v := range spec.Volumes {
		if v.Name == "device-socket" && v.HostPath.Path != "/var/run/cib" {
			t.Errorf("host path %q", v.HostPath.Path)
		}
	}
}

// Every rule of the checker, with its verbs: nothing is created, updated or deleted outside the Lease and the status of a Lab.
func TestCheckerRBACIsExactlyWhatItNeeds(t *testing.T) {
	docs := decodeDocs(t, "templates/checker/rbac.yaml", checksOn...)
	var role, secretRole rbacv1.ClusterRole
	var ns rbacv1.Role
	for _, d := range docs {
		switch {
		case strings.Contains(d, "name: laboratory-checker-secrets\n") && strings.Contains(d, "kind: ClusterRole\n"):
			if err := yaml.Unmarshal([]byte(d), &secretRole); err != nil {
				t.Fatal(err)
			}
		case strings.Contains(d, "kind: ClusterRole\n"):
			if err := yaml.Unmarshal([]byte(d), &role); err != nil {
				t.Fatal(err)
			}
		case strings.Contains(d, "kind: Role\n"):
			if err := yaml.Unmarshal([]byte(d), &ns); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := map[string]string{}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			got[strings.Join(r.APIGroups, ",")+"/"+res] = strings.Join(r.Verbs, ",")
		}
	}
	want := map[string]string{
		"laboratory.cybericebox.com/labs":        "get,list,watch",
		"laboratory.cybericebox.com/labs/status": "patch",
		"/pods":                                  "get,list,watch",
	}
	if len(got) != len(want) {
		t.Errorf("cluster rules %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	// Secrets are read only through the role the operator binds in each lab group namespace: one verb, one resource.
	if len(secretRole.Rules) != 1 || strings.Join(secretRole.Rules[0].Resources, ",") != "secrets" || strings.Join(secretRole.Rules[0].Verbs, ",") != "get" {
		t.Errorf("the secrets role: %+v", secretRole.Rules)
	}
	if len(ns.Rules) != 1 || strings.Join(ns.Rules[0].Resources, ",") != "leases" || strings.Join(ns.Rules[0].Verbs, ",") != "get,create,update,patch" || ns.Namespace != "laboratory-system" {
		t.Errorf("the namespaced role: %+v", ns)
	}
}

func TestCheckerPoliciesAreRenderedWithTheirOptions(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, checksOn...), "-s", "templates/checker/admission-policy.yaml", "-s", "templates/checker/netpol.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"laboratory-checker-status", "laboratory-checker-lease", "kind: CiliumNetworkPolicy", "kube-apiserver", "ingress:\n    - {}"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "toPorts") || strings.Contains(out, "fromEndpoints") {
		t.Error("the checker's policy opens no port and allows no peer")
	}
	for _, off := range [][]string{{"operator.admissionPolicy.enabled=false"}, {"checks.networkPolicy.enabled=false"}} {
		out, _ := helmTemplate(t, append(append([]string{}, checksOn...), "--set", off[0])...)
		if off[0] == "operator.admissionPolicy.enabled=false" && strings.Contains(out, "laboratory-checker-status") {
			t.Error("admission policy rendered while off")
		}
		if off[0] == "checks.networkPolicy.enabled=false" && strings.Contains(out, "kind: CiliumNetworkPolicy\nmetadata:\n  name: laboratory-checker") {
			t.Error("network policy rendered while off")
		}
	}
}

// The DaemonSet is a frame: containers and volumes come from named templates, and the lists after the checker's own are empty today, so
// the terminal part is added with one template of its own, without rewriting the checker's.
func TestCheckerTemplateHasRoomForASecondContainer(t *testing.T) {
	raw, err := os.ReadFile("../../charts/laboratory/templates/checker/_checker.tpl")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := os.ReadFile("../../charts/laboratory/templates/checker/daemonset.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"laboratory.checker.container", "laboratory.checker.extraContainers", "laboratory.checker.volumes", "laboratory.checker.extraVolumes"} {
		if !strings.Contains(string(raw), `define "`+name+`"`) || !strings.Contains(string(ds), `"`+name+`"`) {
			t.Errorf("template %s is missing or not used by the DaemonSet", name)
		}
	}
}

// The agent reports checks as available only while the DaemonSet exists, and may read exactly that one object, only while the option is on.
func TestAgentMayReadOnlyTheCheckerDaemonSet(t *testing.T) {
	var role rbacv1.Role
	render(t, "templates/agent/role-checker.yaml", &role, checksOn...)
	if len(role.Rules) != 1 || strings.Join(role.Rules[0].ResourceNames, ",") != "laboratory-checker" || strings.Join(role.Rules[0].Verbs, ",") != "get" || strings.Join(role.Rules[0].Resources, ",") != "daemonsets" {
		t.Errorf("rules %+v", role.Rules)
	}
	out, err := helmTemplate(t, agentSet...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "laboratory-agent-checker") {
		t.Error("the agent's right to read the checker is rendered while the option is off")
	}
}
