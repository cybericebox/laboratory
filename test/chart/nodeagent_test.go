package chart_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

var baseSet = []string{
	"--namespace", "laboratory-system",
	"--set", "operator.publicVPNEndpoint=vpn.example.com:51820",
	"--set", "operator.baseDomain=lab.example.com",
	"--set", "operator.supportEmail=support@example.com",
}

func helmTemplate(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := append([]string{"template", "x", "../../charts/laboratory"}, baseSet...)
	args = append(args, extra...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

func nodeAgent(t *testing.T, extra ...string) appsv1.DaemonSet {
	t.Helper()
	out, err := helmTemplate(t, append(extra, "-s", "templates/node-agent/daemonset.yaml")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var ds appsv1.DaemonSet
	if err := yaml.Unmarshal([]byte(out), &ds); err != nil {
		t.Fatalf("decode DaemonSet: %v\n%s", err, out)
	}
	return ds
}

func initByName(spec corev1.PodSpec, name string) (int, *corev1.Container) {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return i, &spec.InitContainers[i]
		}
	}
	return -1, nil
}

func TestNodeAgentRunsOVSAsSidecarAfterHostPrep(t *testing.T) {
	spec := nodeAgent(t).Spec.Template.Spec

	prepIdx, prep := initByName(spec, "host-prep")
	ovsIdx, ovs := initByName(spec, "ovs")
	if prep == nil || ovs == nil {
		t.Fatalf("host-prep and ovs must be init containers, got %+v", spec.InitContainers)
	}
	if prepIdx >= ovsIdx {
		t.Errorf("host-prep (%d) must run before ovs (%d)", prepIdx, ovsIdx)
	}
	if ovs.RestartPolicy == nil || *ovs.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("ovs must be a native sidecar (restartPolicy Always)")
	}
	if ovs.StartupProbe == nil {
		t.Errorf("ovs needs a startupProbe: the node-agent waits for it")
	}
	if !spec.HostNetwork {
		t.Errorf("hostNetwork must be true")
	}
	for _, c := range []*corev1.Container{prep, ovs} {
		if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
			t.Errorf("%s must be privileged", c.Name)
		}
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Name != "node-agent" {
		t.Errorf("node-agent must be the only regular container, got %+v", spec.Containers)
	}
}

func TestNodeAgentHostPathVolumes(t *testing.T) {
	spec := nodeAgent(t).Spec.Template.Spec
	want := map[string]string{
		"ovs-run":     "/run/openvswitch",
		"ovs-db":      "/var/lib/cybericebox/openvswitch",
		"lib-modules": "/lib/modules",
	}
	got := map[string]string{}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			got[v.Name] = v.HostPath.Path
		}
		if v.Name == "ovs-run" && v.EmptyDir != nil {
			t.Errorf("ovs-run must be a hostPath, not emptyDir")
		}
	}
	for name, path := range want {
		if got[name] != path {
			t.Errorf("volume %s hostPath = %q, want %q", name, got[name], path)
		}
	}

	_, ovs := initByName(spec, "ovs")
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ovs.VolumeMounts {
		mounts[m.Name] = m
	}
	if mounts["ovs-db"].MountPath != "/etc/openvswitch" {
		t.Errorf("ovs-db must be mounted at /etc/openvswitch")
	}
	if m := mounts["lib-modules"]; m.MountPath != "/lib/modules" || !m.ReadOnly {
		t.Errorf("/lib/modules must be mounted read-only, got %+v", m)
	}
	agent := spec.Containers[0]
	found := false
	for _, m := range agent.VolumeMounts {
		found = found || (m.Name == "ovs-run" && m.MountPath == "/run/openvswitch")
	}
	if !found {
		t.Errorf("node-agent must share /run/openvswitch with ovs")
	}
}

func TestHostPrepScript(t *testing.T) {
	spec := nodeAgent(t).Spec.Template.Spec
	_, prep := initByName(spec, "host-prep")
	script := strings.Join(prep.Args, "\n")
	for _, want := range []string{
		"set -eu",
		`modprobe "openvswitch"`, `modprobe "geneve"`, `modprobe "wireguard"`,
		`echo "1" > /proc/sys/net/ipv4/ip_forward`,
		`echo "0" > /proc/sys/net/ipv4/conf/all/rp_filter`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("host-prep script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "|| true") {
		t.Errorf("host-prep must fail loudly, no fallbacks:\n%s", script)
	}
}

func TestHostPrepValuesAreConfigurable(t *testing.T) {
	spec := nodeAgent(t,
		"--set", "nodeAgent.hostPrep.modules={openvswitch,vxlan}",
		"--set-string", "nodeAgent.hostPrep.sysctls.net\\.ipv4\\.ip_forward=1",
	).Spec.Template.Spec
	_, prep := initByName(spec, "host-prep")
	script := strings.Join(prep.Args, "\n")
	if !strings.Contains(script, `modprobe "vxlan"`) || strings.Contains(script, "wireguard") {
		t.Errorf("modules list not applied:\n%s", script)
	}
}

func TestNoHostOVSMode(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/node-agent/daemonset.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "emptyDir") {
		t.Errorf("OVS state must not live in an emptyDir:\n%s", out)
	}
}

func TestOVSSocketMustBeInsideRunDir(t *testing.T) {
	out, err := helmTemplate(t, "--set", "nodeAgent.ovsSocket=/tmp/db.sock")
	if err == nil {
		t.Fatalf("expected the chart to refuse a socket outside the run dir")
	}
	if !strings.Contains(out, "nodeAgent.ovsSocket") {
		t.Errorf("unexpected error: %s", out)
	}
}

// C-3: no automatic fallback CNI config; it is an explicit opt-in.
func TestCNIFallbackIsOptIn(t *testing.T) {
	has := func(ds appsv1.DaemonSet) (string, bool) {
		_, init := initByName(ds.Spec.Template.Spec, "install-cni-conf")
		if init == nil {
			t.Fatal("no install-cni-conf init container")
		}
		v, ok := envOf(*init)["CNI_FALLBACK_TIMEOUT"]
		return v.Value, ok
	}
	if v, ok := has(nodeAgent(t)); ok {
		t.Errorf("CNI_FALLBACK_TIMEOUT = %q by default: the init container must wait for the real CNI", v)
	}
	if v, ok := has(nodeAgent(t, "--set", "nodeAgent.cniFallbackTimeout=10m")); !ok || v != "10m" {
		t.Errorf("opt-in not applied: %q %v", v, ok)
	}
}

// C-18: the node-agent runs where lab pods can run, and lab pods run only where a ready node-agent has marked its node.
func TestNodeAgentFollowsLabPlacementAndLabPodsNeedItsLabel(t *testing.T) {
	const ready = "laboratory.cybericebox.com/node-agent-ready"
	ds := nodeAgent(t)
	if ds.Spec.Template.Spec.Affinity != nil {
		t.Errorf("no control-plane exclusion any more: %+v", ds.Spec.Template.Spec.Affinity)
	}
	if len(ds.Spec.Template.Spec.NodeSelector) != 0 {
		t.Errorf("default node selector: %v", ds.Spec.Template.Spec.NodeSelector)
	}
	tols := ds.Spec.Template.Spec.Tolerations
	if len(tols) != 1 || tols[0].Key != "node.cilium.io/agent-not-ready" {
		t.Errorf("by default only Cilium's agent-not-ready taint is tolerated: %v", tols)
	}

	set := []string{"--set", "labWorkloads.nodeSelector.pool=labs",
		"--set", "labWorkloads.tolerations[0].key=node-role.kubernetes.io/control-plane", "--set", "labWorkloads.tolerations[0].operator=Exists", "--set", "labWorkloads.tolerations[0].effect=NoSchedule"}
	ds = nodeAgent(t, set...)
	if ds.Spec.Template.Spec.NodeSelector["pool"] != "labs" {
		t.Errorf("the node-agent follows labWorkloads.nodeSelector: %v", ds.Spec.Template.Spec.NodeSelector)
	}
	found := false
	for _, tol := range ds.Spec.Template.Spec.Tolerations {
		found = found || tol.Key == "node-role.kubernetes.io/control-plane"
	}
	if !found {
		t.Errorf("a control-plane node that takes labs must get a node-agent: %v", ds.Spec.Template.Spec.Tolerations)
	}

	for name, args := range map[string][]string{"default": nil, "custom": set} {
		var cm corev1.ConfigMap
		render(t, "templates/operator/configmap.yaml", &cm, args...)
		var sel map[string]string
		if err := json.Unmarshal([]byte(cm.Data["LAB_NODE_SELECTOR"]), &sel); err != nil {
			t.Fatal(err)
		}
		if sel[ready] != "true" || (name == "custom" && sel["pool"] != "labs") || (name == "default" && len(sel) != 1) {
			t.Errorf("%s: LAB_NODE_SELECTOR = %v", name, sel)
		}
		var dep appsv1.Deployment
		render(t, "templates/agent/deployment.yaml", &dep, append(args, "--set", "agent.enabled=true", "--set", "agent.domain=a.example.com")...)
		agentSel := envOf(dep.Spec.Template.Spec.Containers[0])["AGENT_LAB_NODE_SELECTOR"].Value
		if agentSel != cm.Data["LAB_NODE_SELECTOR"] {
			t.Errorf("%s: the agent and the operator must use the same selector: %s vs %s", name, agentSel, cm.Data["LAB_NODE_SELECTOR"])
		}
	}
	// the node-agent may label its own node, nothing else of nodes
	out, err := helmTemplate(t, "-s", "templates/node-agent/clusterrole.yaml")
	if err != nil || !strings.Contains(out, "patch") {
		t.Errorf("the node-agent needs to patch its node's label: %v\n%s", err, out)
	}
}

// B-3: production uses exact tags: latest is refused unless the dev flag is on, and the pull policy follows the tag.
func TestLatestTagIsRefusedAndPullPolicyFollowsTheTag(t *testing.T) {
	out, err := helmTemplate(t, "--set", "operator.image.tag=latest", "-s", "templates/operator/deployment.yaml")
	if err == nil || !strings.Contains(out, "operator.image.tag") || !strings.Contains(out, "latest") {
		t.Errorf("latest must be refused: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "vpn.image.tag=latest", "-s", "templates/operator/deployment.yaml")
	if err == nil || !strings.Contains(out, "vpn.image.tag") {
		t.Errorf("the VPN image too: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "agent.image.tag=latest", "--set", "development.allowLatestTags=true", "--set", "nodeAgent.image.tag=latest", "-s", "templates/node-agent/daemonset.yaml")
	if err != nil || !strings.Contains(out, "imagePullPolicy: Always") || strings.Contains(out, "imagePullPolicy: IfNotPresent") {
		t.Errorf("with the dev flag latest renders and is pulled Always: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "nodeAgent.image.tag=v1.2.3", "-s", "templates/node-agent/daemonset.yaml")
	if err != nil || !strings.Contains(out, "imagePullPolicy: IfNotPresent") || strings.Contains(out, "imagePullPolicy: Always") {
		t.Errorf("an exact tag is pulled IfNotPresent: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "nodeAgent.image.pullPolicy=Never", "-s", "templates/node-agent/daemonset.yaml")
	if err != nil || !strings.Contains(out, "imagePullPolicy: Never") {
		t.Errorf("an explicit pull policy wins: %v", err)
	}
}
