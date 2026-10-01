package chart_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func operatorConfig(t *testing.T, extra ...string) map[string]string {
	t.Helper()
	out, err := helmTemplate(t, append(extra, "-s", "templates/operator/configmap.yaml")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(out), &cm); err != nil {
		t.Fatalf("decode ConfigMap: %v\n%s", err, out)
	}
	return cm.Data
}

func TestSchedulerDefaultsReachTheOperator(t *testing.T) {
	want := map[string]string{
		"SCHEDULER_ENABLED":           "true",
		"SCHEDULER_MAX_PODS":          "20",
		"SCHEDULER_STARTUP_TIMEOUT":   "5m",
		"SCHEDULER_RESTART_THRESHOLD": "5",
		"SCHEDULER_HEADROOM_PERCENT":  "10",
		"SCHEDULER_RESOURCE_CHECK":    "true",
		"SCHEDULER_PREPULL":           "true",
		"SCHEDULER_PREPULL_TIMEOUT":   "5m",
		"DEVICE_DEFAULT_CPU":          "250m",
		"DEVICE_DEFAULT_MEMORY":       "256Mi",
	}
	got := operatorConfig(t)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "LAUNCH_") {
			t.Errorf("old launch setting %s is still rendered", k)
		}
	}
}

func TestSchedulerValuesOverride(t *testing.T) {
	got := operatorConfig(t,
		"--set", "scheduler.enabled=false",
		"--set", "scheduler.maxPods=50",
		"--set", "scheduler.startupTimeout=90s",
		"--set", "scheduler.restartThreshold=3",
		"--set", "scheduler.headroomPercent=25",
		"--set", "scheduler.resourceCheck=false",
		"--set", "scheduler.prepull.enabled=false",
		"--set", "scheduler.prepull.timeout=10m",
		"--set", "scheduler.deviceDefaults.cpu=500m",
		"--set", "scheduler.deviceDefaults.memory=1Gi",
	)
	want := map[string]string{
		"SCHEDULER_ENABLED": "false", "SCHEDULER_MAX_PODS": "50", "SCHEDULER_STARTUP_TIMEOUT": "90s",
		"SCHEDULER_RESTART_THRESHOLD": "3", "SCHEDULER_HEADROOM_PERCENT": "25", "SCHEDULER_RESOURCE_CHECK": "false",
		"SCHEDULER_PREPULL": "false", "SCHEDULER_PREPULL_TIMEOUT": "10m",
		"DEVICE_DEFAULT_CPU": "500m", "DEVICE_DEFAULT_MEMORY": "1Gi",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSchedulerRejectsBadValues(t *testing.T) {
	for _, set := range []string{"scheduler.maxPods=-1", "scheduler.headroomPercent=100", "scheduler.headroomPercent=-5", "scheduler.restartThreshold=0"} {
		out, err := helmTemplate(t, "--set", set)
		if err == nil {
			t.Errorf("%s must be refused", set)
		} else if !strings.Contains(out, "scheduler.") {
			t.Errorf("%s: error does not name the value: %s", set, out)
		}
	}
}

// The operator reads nodes and runs the prepull DaemonSets, so its ClusterRole needs both.
func TestOperatorMayReadNodesAndManagePrepullDaemonSets(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/operator/clusterrole.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal([]byte(out), &role); err != nil {
		t.Fatal(err)
	}
	can := func(group, resource, verb string) bool {
		for _, r := range role.Rules {
			if !contains(r.APIGroups, group) || !contains(r.Resources, resource) {
				continue
			}
			if contains(r.Verbs, verb) {
				return true
			}
		}
		return false
	}
	for _, c := range [][3]string{
		{"", "nodes", "list"}, {"", "nodes", "watch"},
		{"apps", "daemonsets", "create"}, {"apps", "daemonsets", "delete"}, {"apps", "daemonsets", "list"}, {"apps", "daemonsets", "watch"},
		{"", "pods", "list"},
	} {
		if !can(c[0], c[1], c[2]) {
			t.Errorf("operator cannot %s %s/%s", c[2], c[0], c[1])
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
