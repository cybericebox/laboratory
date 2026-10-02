package chart_test

import (
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
