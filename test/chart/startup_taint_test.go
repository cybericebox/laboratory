package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func tolerates(tolerations []corev1.Toleration, key string) bool {
	for _, t := range tolerations {
		if t.Key == key && t.Operator == corev1.TolerationOpExists && (t.Effect == corev1.TaintEffectNoSchedule || key != "cybericebox.io/node-starting") {
			return true
		}
	}
	return false
}

// A new lab worker carries the startup taint until its node-agent removes it: the DaemonSets that make the node usable must tolerate it,
// whatever the lab placement says, and a Deployment must not.
func TestStartupTaintToleratedByTheNodeDaemonSets(t *testing.T) {
	const key = "cybericebox.io/node-starting"
	for _, set := range [][]string{nil, {"--set", "labWorkloads.nodeSelector.cybericebox\\.io/lab=true"}} {
		if ds := nodeAgent(t, set...); !tolerates(ds.Spec.Template.Spec.Tolerations, key) {
			t.Errorf("node-agent does not tolerate %s (%v)", key, set)
		}
		var wg appsv1.DaemonSet
		render(t, "templates/wg/daemonset.yaml", &wg, append(set, "--set", "proxy.wg.enabled=true")...)
		if !tolerates(wg.Spec.Template.Spec.Tolerations, key) {
			t.Errorf("wg demux does not tolerate %s (%v)", key, set)
		}
	}
	var wg appsv1.DaemonSet
	render(t, "templates/wg/daemonset.yaml", &wg, "--set", "proxy.wg.enabled=true", "--set", "labWorkloads.tolerations[0].key=cybericebox.io/lab", "--set", "labWorkloads.tolerations[0].operator=Exists")
	if !tolerates(wg.Spec.Template.Spec.Tolerations, key) || !tolerates(wg.Spec.Template.Spec.Tolerations, "cybericebox.io/lab") {
		t.Errorf("the startup toleration must come on top of the lab tolerations: %+v", wg.Spec.Template.Spec.Tolerations)
	}
	var proxyDS appsv1.DaemonSet
	render(t, "templates/proxy/daemonset.yaml", &proxyDS, "--set", "proxy.mode=daemonset")
	if !tolerates(proxyDS.Spec.Template.Spec.Tolerations, key) {
		t.Error("the proxy DaemonSet does not tolerate the startup taint")
	}
	var proxyDep appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &proxyDep)
	if tolerates(proxyDep.Spec.Template.Spec.Tolerations, key) {
		t.Error("the proxy Deployment must not land on a starting node")
	}
}

func TestNodeAgentMayRemoveOnlyTheStartupTaint(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/node-agent/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"t.key != 'cybericebox.io/node-starting'", "object.spec.?unschedulable == oldObject.spec.?unschedulable"} {
		if !strings.Contains(out, want) {
			t.Errorf("admission policy lacks %q", want)
		}
	}
	if strings.Contains(out, "may not change the spec of a node (taints") {
		t.Error("the old blanket spec rule is back")
	}
}
