package chart_test

import (
	"regexp"
	"testing"
)

// The state limits against abuse reach the node-agent and the agent from the chart.
func TestStateAbuseLimitsAreValues(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "--set", "devices.statePersistence.enabled=true", "--set", "devices.statePersistence.maxWatchedDirs=500",
		"--set", "devices.statePersistence.tenantQuota=3Gi", "-s", "templates/node-agent/daemonset.yaml", "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for name, val := range map[string]string{"STATE_MAX_WATCH_DIRS": "500", "STATE_TENANT_QUOTA": "3Gi", "STATE_MAX_ENTRIES": "100000"} {
		if !regexp.MustCompile(`name: ` + name + `\s+value: "` + val + `"`).MatchString(out) {
			t.Errorf("%s = %s missing", name, val)
		}
	}
}
