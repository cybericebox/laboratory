package chart_test

import (
	"os"
	"regexp"
	"testing"
)

// nodePatchRule finds the ClusterRole rule that patches nodes in a rendered chart.
func nodePatchRule(t *testing.T, out string) bool {
	t.Helper()
	for _, d := range docs(t, out) {
		if d["kind"] != "ClusterRole" {
			continue
		}
		for _, r := range rulesOf(t, d) {
			if has(r.Resources, "nodes") && has(r.Verbs, "patch") {
				return true
			}
		}
	}
	return false
}

// The node writes of node-state and of the node-agent are confined by admission policies; RBAC cannot say "one annotation of one
// node". The rule exists in the rendered chart (the policies are on by default and the chart refuses to render these workloads without
// them) and the template keeps it inside the policy switch, so no render can ever carry the patch without the policy.
func TestNodePatchIsGatedByTheAdmissionPolicy(t *testing.T) {
	gate := regexp.MustCompile(`(?s)\{\{-\s*if \.Values\.operator\.admissionPolicy\.enabled\s*\}\}\s*- apiGroups: \[ "" \]\s*resources: \[ nodes \]\s*verbs: \[ patch \]\s*\{\{-\s*end\s*\}\}`)
	for _, c := range []struct{ name, file string }{
		{"node-state", "templates/node-state/rbac.yaml"},
		{"node-agent", "templates/node-agent/clusterrole.yaml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, err := os.ReadFile("../../charts/laboratory/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			if !gate.Match(src) {
				t.Errorf("the nodes patch rule of %s must sit inside `if .Values.operator.admissionPolicy.enabled`", c.file)
			}
			out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", c.file)
			if err != nil {
				t.Fatal(err)
			}
			if !nodePatchRule(t, out) {
				t.Errorf("%s lost its nodes patch rule with the admission policy on", c.file)
			}
		})
	}
}
