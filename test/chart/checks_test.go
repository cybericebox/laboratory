package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

// agentEnv is the environment of the management agent container as the chart renders it.
func agentEnv(t *testing.T, extra ...string) map[string]string {
	t.Helper()
	var dep appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &dep, append(append([]string{}, agentSet...), extra...)...)
	env := map[string]string{}
	for k, v := range envOf(dep.Spec.Template.Spec.Containers[0]) {
		env[k] = v.Value
	}
	return env
}

// The checks and the terminal are two independent options, both off by default; the agent and the operator read the same values.
func TestChecksAndTerminalAreOffByDefault(t *testing.T) {
	op, ag := operatorConfig(t), agentEnv(t)
	for _, k := range []string{"CHECKS_ENABLED", "TERMINAL_ENABLED"} {
		if op[k] != "false" || ag[k] != "false" {
			t.Errorf("%s: operator %q, agent %q, want false in both", k, op[k], ag[k])
		}
	}
}

func TestChecksAndTerminalSwitchesAreIndependent(t *testing.T) {
	for _, c := range []struct {
		set                    string
		wantChecks, wantTermin string
	}{
		{"checks.enabled=true", "true", "false"},
		{"terminal.enabled=true", "false", "true"},
	} {
		op, ag := operatorConfig(t, "--set", c.set), agentEnv(t, "--set", c.set)
		for name, got := range map[string]string{"operator checks": op["CHECKS_ENABLED"], "agent checks": ag["CHECKS_ENABLED"]} {
			if got != c.wantChecks {
				t.Errorf("--set %s: %s = %q, want %q", c.set, name, got, c.wantChecks)
			}
		}
		for name, got := range map[string]string{"operator terminal": op["TERMINAL_ENABLED"], "agent terminal": ag["TERMINAL_ENABLED"]} {
			if got != c.wantTermin {
				t.Errorf("--set %s: %s = %q, want %q", c.set, name, got, c.wantTermin)
			}
		}
	}
}

// The terminal has no component yet: its switch alone renders nothing, and with both on only the checker is rendered.
func TestNoTerminalComponentYet(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "checks.enabled=true", "--set", "terminal.enabled=true")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "laboratory-terminal") {
		t.Error("the chart renders a terminal component, which is not part of this step")
	}
	if !strings.Contains(out, "laboratory-checker") {
		t.Error("checks.enabled=true must render the checker")
	}
}
