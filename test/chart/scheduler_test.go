package chart_test

import (
	"os"
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
		"SCHEDULER_ENABLED":                  "true",
		"SCHEDULER_MAX_PODS":                 "20",
		"SCHEDULER_STARTUP_TIMEOUT":          "5m",
		"SCHEDULER_RESTART_THRESHOLD":        "5",
		"SCHEDULER_PLATFORM_RESERVE_PERCENT": "10", "SCHEDULER_PLATFORM_RESERVE_CPU": "0", "SCHEDULER_PLATFORM_RESERVE_MEMORY": "0",
		"SCHEDULER_RESOURCE_CHECK":  "true",
		"SCHEDULER_PREPULL":         "true",
		"SCHEDULER_PREPULL_TIMEOUT": "5m",
		"DEVICE_DEFAULT_CPU":        "100m",
		"DEVICE_DEFAULT_MEMORY":     "256Mi",
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
		"--set", "scheduler.platformReservePercent=25", "--set", "scheduler.platformReserveCpu=300m", "--set", "scheduler.platformReserveMemory=1Gi",
		"--set", "scheduler.resourceCheck=false",
		"--set", "scheduler.prepull.enabled=false",
		"--set", "scheduler.prepull.timeout=10m",
		"--set", "limits.device.defaultCpu=400m",
		"--set", "limits.device.defaultMemory=1Gi",
	)
	want := map[string]string{
		"SCHEDULER_ENABLED": "false", "SCHEDULER_MAX_PODS": "50", "SCHEDULER_STARTUP_TIMEOUT": "90s",
		"SCHEDULER_RESTART_THRESHOLD": "3", "SCHEDULER_PLATFORM_RESERVE_PERCENT": "25", "SCHEDULER_PLATFORM_RESERVE_CPU": "300m", "SCHEDULER_PLATFORM_RESERVE_MEMORY": "1Gi", "SCHEDULER_RESOURCE_CHECK": "false",
		"SCHEDULER_PREPULL": "false", "SCHEDULER_PREPULL_TIMEOUT": "10m",
		"DEVICE_DEFAULT_CPU": "400m", "DEVICE_DEFAULT_MEMORY": "1Gi",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSchedulerRejectsBadValues(t *testing.T) {
	for _, set := range []string{"scheduler.maxPods=-1", "scheduler.platformReservePercent=100", "scheduler.platformReservePercent=-5", "scheduler.headroomPercent=10", "scheduler.restartThreshold=0"} {
		out, err := helmTemplate(t, "--set", set)
		if err == nil {
			t.Errorf("%s must be refused", set)
		} else if !strings.Contains(out, "scheduler.") {
			t.Errorf("%s: error does not name the value: %s", set, out)
		}
	}
}

// The operator reads nodes cluster-wide and makes the prepull requests (ImagePull).
func TestOperatorMayReadNodesAndManagePrepullRequests(t *testing.T) {
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
		{"laboratory.cybericebox.com", "imagepulls", "create"}, {"laboratory.cybericebox.com", "imagepulls", "delete"},
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

// The agent applies the operator's platform reserve to the per-node room it reports, so it gets the same values.
func TestAgentGetsThePlatformReserve(t *testing.T) {
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com",
		"--set", "scheduler.platformReservePercent=15", "--set", "scheduler.platformReserveCpu=250m", "--set", "scheduler.platformReserveMemory=512Mi",
		"-s", "templates/agent/deployment.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{
		"name: SCHEDULER_PLATFORM_RESERVE_PERCENT\n              value: \"15\"",
		"name: SCHEDULER_PLATFORM_RESERVE_CPU\n              value: \"250m\"",
		"name: SCHEDULER_PLATFORM_RESERVE_MEMORY\n              value: \"512Mi\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the agent deployment lacks %q:\n%s", want, out)
		}
	}
}

// Maintenance windows: the agent reads them (read only), and the CRD ships with the chart.
func TestAgentMayReadMaintenanceWindows(t *testing.T) {
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com", "-s", "templates/agent/clusterrole.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	i := strings.Index(out, `resources: [ "maintenancewindows" ]`)
	if i < 0 {
		t.Fatalf("the agent cluster role lacks maintenancewindows:\n%s", out)
	}
	rule := out[i : i+120]
	if strings.Contains(rule, "create") || strings.Contains(rule, "delete") || strings.Contains(rule, "update") {
		t.Errorf("the agent must only read maintenance windows: %s", rule)
	}
	if _, err := os.Stat("../../charts/laboratory/crds/laboratory.cybericebox.com_maintenancewindows.yaml"); err != nil {
		t.Errorf("the CRD is not in the chart: %v", err)
	}
}

// The agent reports its capacity net of the hidden packing reserve: it gets the chart value.
func TestAgentGetsThePackingReserve(t *testing.T) {
	out, err := helmTemplate(t, "--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com",
		"--set", "scheduler.packingReservePercent=20", "-s", "templates/agent/deployment.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if want := "name: SCHEDULER_PACKING_RESERVE_PERCENT\n              value: \"20\""; !strings.Contains(out, want) {
		t.Errorf("the agent deployment lacks %q:\n%s", want, out)
	}
	if out, _ = helmTemplate(t, "--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com", "-s", "templates/agent/deployment.yaml"); !strings.Contains(out, "value: \"15\"") {
		t.Errorf("the default packing reserve is 15%%:\n%s", out)
	}
}

// Lab pods use a bin-packing scheduler profile: a second kube-scheduler whose profile scores NodeResourcesFit MostAllocated and
// leaves out the spreading score, and the operator puts its name into the pods of labs. Off: the default scheduler, no extra
// scheduler.
func TestLabSchedulerIsABinPackingProfile(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/lab-scheduler/scheduler.yaml", "-s", "templates/lab-scheduler/configmap.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{
		"schedulerName: laboratory-binpack", "type: MostAllocated", "name: NodeResourcesBalancedAllocation", "kind: Deployment",
		"resourceName: laboratory-scheduler", "name: system:kube-scheduler",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the lab scheduler lacks %q", want)
		}
	}
	cfg := operatorConfig(t)
	if cfg["LAB_SCHEDULER_NAME"] != "laboratory-binpack" {
		t.Errorf("the operator does not name the profile: %q", cfg["LAB_SCHEDULER_NAME"])
	}
	// off: nothing is rendered and the operator keeps the default scheduler
	off := operatorConfig(t, "--set", "labScheduler.enabled=false")
	if _, set := off["LAB_SCHEDULER_NAME"]; set {
		t.Errorf("a disabled lab scheduler must not be named: %q", off["LAB_SCHEDULER_NAME"])
	}
	if out, err = helmTemplate(t, "--set", "labScheduler.enabled=false", "-s", "templates/lab-scheduler/scheduler.yaml"); err == nil && strings.Contains(out, "kind: Deployment") {
		t.Errorf("a disabled lab scheduler renders a Deployment:\n%s", out)
	}
}

// Lab pods need no graceful drain: the operator gets a short terminationGracePeriodSeconds for them, so a deleted lab does not
// wait the 30 s default.
func TestLabPodsGetAShortTerminationGrace(t *testing.T) {
	if got := operatorConfig(t)["LAB_POD_TERMINATION_GRACE_SECONDS"]; got != "5" {
		t.Errorf("LAB_POD_TERMINATION_GRACE_SECONDS = %q, want 5", got)
	}
	if got := operatorConfig(t, "--set", "operator.labPodTerminationGraceSeconds=2")["LAB_POD_TERMINATION_GRACE_SECONDS"]; got != "2" {
		t.Errorf("the value must reach the operator: %q", got)
	}
}
