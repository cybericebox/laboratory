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
	if sc.Capabilities == nil || strings.Join(capNames(sc.Capabilities.Drop), ",") != "ALL" || strings.Join(capNames(sc.Capabilities.Add), ",") != "DAC_READ_SEARCH" {
		t.Errorf("capabilities %+v: exactly DAC_READ_SEARCH on top of none", sc.Capabilities)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", spec.SecurityContext)
	}
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() || c.Resources.Requests.Memory().IsZero() {
		t.Errorf("the checker has limits of its own: %+v", c.Resources)
	}
	// The host paths: the runtime socket, and the containerd state read-only with propagation.
	hostPaths := map[string]string{}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			hostPaths[v.Name] = v.HostPath.Path
		}
	}
	if len(hostPaths) != 2 || hostPaths["cri-sock"] != "/run/k0s/containerd.sock" || hostPaths["containerd-state"] != "/run/k0s/containerd" {
		t.Errorf("host paths %v", hostPaths)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "containerd-state" && (!m.ReadOnly || m.MountPropagation == nil || *m.MountPropagation != corev1.MountPropagationHostToContainer) {
			t.Errorf("the containerd state must be read-only with HostToContainer: %+v", m)
		}
	}
	env := envOf(c)
	for _, k := range []string{"NODE_NAME", "POD_NAME", "POD_NAMESPACE", "CRI_SOCK", "CONTAINERD_STATE", "CONTAINERD_NAMESPACE", "CHECKER_MAX_CONCURRENT"} {
		if _, ok := env[k]; !ok {
			t.Errorf("env %s missing", k)
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

// The values reach the pod: the state directory, the namespace, the concurrency and the placement of the lab nodes.
func TestCheckerFollowsItsValuesAndTheLabNodes(t *testing.T) {
	ds := checkerDaemonSet(t, "--set", "checks.containerdState=/var/run/c8d", "--set", "checks.containerdNamespace=cri", "--set", "checks.maxConcurrent=9",
		"--set", "nodeAgent.criSocket=/run/c8d.sock", "--set", "labWorkloads.nodeSelector.pool=labs")
	spec := ds.Spec.Template.Spec
	env := envOf(spec.Containers[0])
	if env["CONTAINERD_STATE"].Value != "/var/run/c8d" || env["CONTAINERD_NAMESPACE"].Value != "cri" || env["CHECKER_MAX_CONCURRENT"].Value != "9" || env["CRI_SOCK"].Value != "/run/c8d.sock" {
		t.Errorf("env %v", env)
	}
	if spec.NodeSelector["pool"] != "labs" {
		t.Errorf("node selector %v: the checker runs where the labs run", spec.NodeSelector)
	}
	for _, v := range spec.Volumes {
		if v.Name == "containerd-state" && v.HostPath.Path != "/var/run/c8d" {
			t.Errorf("host path %q", v.HostPath.Path)
		}
	}
}

// Every rule of the checker, with its verbs: nothing is created, updated or deleted outside the Lease and the status of a Lab.
func TestCheckerRBACIsExactlyWhatItNeeds(t *testing.T) {
	docs := decodeDocs(t, "templates/checker/rbac.yaml", checksOn...)
	var role rbacv1.ClusterRole
	var ns rbacv1.Role
	for _, d := range docs {
		switch {
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
		"/secrets":                               "get",
	}
	if len(got) != len(want) {
		t.Errorf("cluster rules %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
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
