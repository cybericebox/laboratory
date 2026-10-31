package chart_test

import (
	"strings"
	"testing"
)

// lifecycle.profile=full is one switch for persistence, the native observation and the four lifecycle capabilities; the default is off.
func TestLifecycleProfileFullSwitchesOnWhatTheLifecycleNeeds(t *testing.T) {
	off := operatorConfig(t)
	for _, k := range []string{"LIFECYCLE_PER_LAB_STOP_AVAILABLE", "LIFECYCLE_CONFIRMED_RUNTIME_AVAILABLE", "LIFECYCLE_RETAINED_RESTART_AVAILABLE", "LIFECYCLE_FULL_GROUP_STOP_AVAILABLE", "RUNTIME_OBSERVATION_ENABLED", "STATE_PERSISTENCE_ENABLED"} {
		if off[k] == "true" {
			t.Errorf("the default must be off: %s = %q", k, off[k])
		}
	}
	on := operatorConfig(t, "--set", "lifecycle.profile=full")
	for k, want := range map[string]string{
		"LIFECYCLE_PER_LAB_STOP_AVAILABLE": "true", "LIFECYCLE_CONFIRMED_RUNTIME_AVAILABLE": "true", "LIFECYCLE_RETAINED_RESTART_AVAILABLE": "true",
		"LIFECYCLE_FULL_GROUP_STOP_AVAILABLE": "true", "RUNTIME_OBSERVATION_ENABLED": "true", "STATE_PERSISTENCE_ENABLED": "true",
		"LIFECYCLE_REQUIRED_SNAPSHOT_AVAILABLE": "false",
	} {
		if on[k] != want {
			t.Errorf("profile full: %s = %q, want %q", k, on[k], want)
		}
	}
}

// The node-agent gets the same: persistence and observation on, the containerd root to check, and a hostPath without the Directory type
// check, so a wrong path reaches the node-agent's own start-up check instead of leaving the pod waiting for its volume.
func TestLifecycleProfileFullConfiguresTheNodeAgent(t *testing.T) {
	out, err := helmTemplate(t, "--set", "lifecycle.profile=full", "--set", "devices.statePersistence.containerdRoot=/var/lib/containerd", "-s", "templates/node-agent/daemonset.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{"name: CONTAINERD_ROOT\n              value: \"/var/lib/containerd\"", "name: RUNTIME_OBSERVATION_ENABLED\n              value: \"true\"", "name: STATE_PERSISTENCE_ENABLED\n              value: \"true\""} {
		if !strings.Contains(out, want) {
			t.Errorf("the node-agent lacks %q", want)
		}
	}
	i := strings.Index(out, "name: containerd-root\n          hostPath:")
	if i < 0 {
		t.Fatal("no containerd-root volume")
	}
	vol := out[i : i+400]
	if strings.Contains(vol, "type: Directory") || !strings.Contains(vol, `type: ""`) {
		t.Errorf("the containerd root volume must have no type check:\n%s", vol)
	}
}

func TestLifecycleProfileRefusesAnUnknownValueAndKeepsTheDependencies(t *testing.T) {
	if out, err := helmTemplate(t, "--set", "lifecycle.profile=half", "-s", "templates/operator/configmap.yaml"); err == nil || !strings.Contains(out, "lifecycle.profile must be empty or") {
		t.Errorf("an unknown profile must fail: %v\n%s", err, out)
	}
	// the profile does not bypass the checks of the dependencies
	if out, err := helmTemplate(t, "--set", "lifecycle.profile=full", "--set", "operator.admissionPolicy.enabled=false", "-s", "templates/operator/configmap.yaml"); err == nil || !strings.Contains(out, "require admission protection") {
		t.Errorf("the profile without the admission policy must fail the existing validation: %v\n%s", err, out)
	}
	if out, err := helmTemplate(t, "--set", "lifecycle.profile=full", "--set", "devices.statePersistence.containerdRoot=relative", "-s", "templates/operator/configmap.yaml"); err == nil || !strings.Contains(out, "containerdRoot must be an absolute") {
		t.Errorf("a relative containerdRoot must fail: %v\n%s", err, out)
	}
}
