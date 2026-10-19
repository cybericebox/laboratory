package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// The budget reaches the operator and the agent under the same names; the node timeout only the operator.
func TestCapacityValuesRenderEnv(t *testing.T) {
	got := operatorConfig(t)
	for k, want := range map[string]string{"CAPACITY_BUDGET_CPU": "", "CAPACITY_BUDGET_MEMORY": "", "CAPACITY_NODE_TIMEOUT": "900s"} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("default %s = %q (set %v), want %q", k, v, ok, want)
		}
	}
	set := append(append([]string{}, agentSet...), "--set", "capacity.budget.cpu=47040m", "--set", "capacity.budget.memory=180Gi",
		"--set", "capacity.nodeTimeoutSeconds=600")
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, set...)
	var dep appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &dep, set...)
	env := envOf(dep.Spec.Template.Spec.Containers[0])
	for k, want := range map[string]string{"CAPACITY_BUDGET_CPU": "47040m", "CAPACITY_BUDGET_MEMORY": "180Gi"} {
		if cm.Data[k] != want || env[k].Value != want {
			t.Errorf("%s: operator %q, agent %q, want %q", k, cm.Data[k], env[k].Value, want)
		}
	}
	if cm.Data["CAPACITY_NODE_TIMEOUT"] != "600s" {
		t.Errorf("CAPACITY_NODE_TIMEOUT = %q", cm.Data["CAPACITY_NODE_TIMEOUT"])
	}
}

func TestCapacityValidateFailsWithHalfABudget(t *testing.T) {
	for _, set := range [][]string{
		{"--set", "capacity.budget.cpu=48"},
		{"--set", "capacity.budget.memory=192Gi"},
		{"--set", "capacity.nodeTimeoutSeconds=0"},
		{"--set", "capacity.maxNodes=3"},
	} {
		if out, err := helmTemplate(t, set...); err == nil || !strings.Contains(out, "capacity.") {
			t.Errorf("%v must be refused: %v\n%s", set, err, out)
		}
	}
	if out, err := helmTemplate(t, "--set", "capacity.budget.cpu=48", "--set", "capacity.budget.memory=192Gi"); err != nil {
		t.Errorf("a full budget renders: %v\n%s", err, out)
	}
}
