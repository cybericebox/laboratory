package chart_test

import (
	"strings"
	"testing"
)

// Activity reports are off by default everywhere they are read, and one value turns them on for the agent, the operator and the proxy.
func TestActivityReportsAreOffByDefaultAndOneValueTurnsThemOn(t *testing.T) {
	// the env map the operator and the agent both read
	if v, ok := operatorConfig(t)["ACTIVITY_REPORTS"]; !ok || v != "false" {
		t.Fatalf("default ACTIVITY_REPORTS = %q (set %v), want false", v, ok)
	}
	if v := operatorConfig(t, "--set", "activityReports.enabled=true")["ACTIVITY_REPORTS"]; v != "true" {
		t.Fatalf("enabled ACTIVITY_REPORTS = %q", v)
	}
	proxyEnv := func(extra ...string) string {
		out, err := helmTemplate(t, append(extra, "-s", "templates/proxy/deployment.yaml")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		i := strings.Index(out, "name: ACTIVITY_REPORTS")
		if i < 0 {
			t.Fatal("the proxy has no ACTIVITY_REPORTS")
		}
		return strings.TrimSpace(strings.SplitN(out[i:], "\n", 3)[1])
	}
	if got := proxyEnv(); got != `value: "false"` {
		t.Fatalf("default proxy env = %s", got)
	}
	if got := proxyEnv("--set", "activityReports.enabled=true"); got != `value: "true"` {
		t.Fatalf("enabled proxy env = %s", got)
	}
}

// Every writer may write its own ring, and the agent may read and clean them: the rules are exact.
func TestActivityReportsRBAC(t *testing.T) {
	want := map[string]struct {
		resource string
		verbs    []string
	}{
		"templates/proxy/clusterrole-reports.yaml":       {"labactivityreports", []string{"get", "create"}},
		"templates/operator/clusterrole-vpn.yaml":        {"labactivityreports", []string{"get", "create", "update"}},
		"templates/agent/clusterrole.yaml":               {"labactivityreports", []string{"get", "list", "watch", "delete"}},
		"templates/operator/clusterrole.yaml":            {"labactivityreports", []string{"get", "create", "update"}},
		"templates/operator/clusterrole-namespaced.yaml": {"", nil},
	}
	for file, w := range want {
		if w.resource == "" {
			continue
		}
		out, err := helmTemplate(t, append(append([]string{}, agentSet...), "-s", file)...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", file, err, out)
		}
		found := false
		for _, doc := range docs(t, out) {
			for _, r := range rulesOf(t, doc) {
				if !has(r.Resources, w.resource) {
					continue
				}
				found = true
				for _, v := range w.verbs {
					if !has(r.Verbs, v) {
						t.Errorf("%s: %s lacks verb %q: %+v", file, w.resource, v, r)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s has no rule for %s", file, w.resource)
		}
	}
	// the proxy writes the status but may not list, delete or read other replicas' rings
	out, err := helmTemplate(t, "-s", "templates/proxy/clusterrole-reports.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs(t, out) {
		for _, r := range rulesOf(t, doc) {
			if has(r.Resources, "labactivityreports") && (has(r.Verbs, "list") || has(r.Verbs, "delete") || has(r.Verbs, "watch")) {
				t.Errorf("the proxy may not list, watch or delete reports: %+v", r)
			}
		}
	}
}
