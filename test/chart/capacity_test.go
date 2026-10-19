package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// The static maximum reaches the operator and the agent under the same names; the node timeout only the operator.
func TestCapacityValuesRenderEnv(t *testing.T) {
	got := operatorConfig(t)
	for k, want := range map[string]string{"CAPACITY_MAX_NODES": "0", "CAPACITY_ADDED_NODE_CPU": "", "CAPACITY_ADDED_NODE_MEMORY": "", "CAPACITY_NODE_TIMEOUT": "900s"} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("default %s = %q (set %v), want %q", k, v, ok, want)
		}
	}
	set := append(append([]string{}, agentSet...), "--set", "capacity.maxNodes=6", "--set", "capacity.addedNode.cpu=3920m",
		"--set", "capacity.addedNode.memory=15Gi", "--set", "capacity.nodeTimeoutSeconds=600")
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, set...)
	var dep appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &dep, set...)
	env := envOf(dep.Spec.Template.Spec.Containers[0])
	for k, want := range map[string]string{"CAPACITY_MAX_NODES": "6", "CAPACITY_ADDED_NODE_CPU": "3920m", "CAPACITY_ADDED_NODE_MEMORY": "15Gi"} {
		if cm.Data[k] != want || env[k].Value != want {
			t.Errorf("%s: operator %q, agent %q, want %q", k, cm.Data[k], env[k].Value, want)
		}
	}
	if cm.Data["CAPACITY_NODE_TIMEOUT"] != "600s" {
		t.Errorf("CAPACITY_NODE_TIMEOUT = %q", cm.Data["CAPACITY_NODE_TIMEOUT"])
	}
}

func TestCapacityValidateFailsWithoutAddedNode(t *testing.T) {
	for _, set := range [][]string{
		{"--set", "capacity.maxNodes=3"},
		{"--set", "capacity.maxNodes=3", "--set", "capacity.addedNode.cpu=4"},
		{"--set", "capacity.maxNodes=-1"},
		{"--set", "capacity.nodeTimeoutSeconds=0"},
	} {
		if out, err := helmTemplate(t, set...); err == nil || !strings.Contains(out, "capacity.") {
			t.Errorf("%v must be refused: %v\n%s", set, err, out)
		}
	}
	if out, err := helmTemplate(t, "--set", "capacity.maxNodes=3", "--set", "capacity.addedNode.cpu=4", "--set", "capacity.addedNode.memory=16Gi"); err != nil {
		t.Errorf("a full maximum renders: %v\n%s", err, out)
	}
}

// The operator and the agent read the DaemonSets (the room they take on a node still to come), and nothing more of them.
func TestCapacityReadsDaemonSetsOnly(t *testing.T) {
	for tpl, want := range map[string][]string{
		"templates/operator/clusterrole.yaml": {"get", "list", "watch"},
		"templates/agent/clusterrole.yaml":    {"list"},
	} {
		out, err := helmTemplate(t, append(append([]string{}, agentSet...), "-s", tpl)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		found := false
		for _, d := range docs(t, out) {
			for _, r := range rulesOf(t, d) {
				if !has(r.Resources, "daemonsets") {
					continue
				}
				found = true
				if len(r.Verbs) != len(want) {
					t.Errorf("%s: daemonsets verbs %v, want %v", tpl, r.Verbs, want)
				}
				for _, v := range want {
					if !has(r.Verbs, v) {
						t.Errorf("%s: daemonsets verbs %v, want %v", tpl, r.Verbs, want)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no daemonsets rule", tpl)
		}
	}
}
