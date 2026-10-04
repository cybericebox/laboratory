package chart_test

import (
	"strings"
	"testing"
)

type rule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
	Verbs         []string `json:"verbs"`
}

func rulesOf(t *testing.T, doc map[string]any) []rule {
	t.Helper()
	raw, _ := doc["rules"].([]any)
	var out []rule
	for _, r := range raw {
		m := r.(map[string]any)
		var rl rule
		toStrings := func(key string) []string {
			var s []string
			for _, v := range m[key].([]any) {
				s = append(s, v.(string))
			}
			return s
		}
		rl.APIGroups, rl.Resources, rl.Verbs = toStrings("apiGroups"), toStrings("resources"), toStrings("verbs")
		if _, ok := m["resourceNames"]; ok {
			rl.ResourceNames = toStrings("resourceNames")
		}
		out = append(out, rl)
	}
	return out
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// The operator reads no Secret cluster-wide and writes nothing that lives in a namespace (Secrets, Deployments, pods,
// Services, NetworkPolicies...) with its cluster-wide role: that comes from RoleBindings in the namespaces it works in.
func TestOperatorClusterRoleIsLeastPrivilege(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/operator/clusterrole.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	docs := docs(t, out)
	if len(docs) != 1 {
		t.Fatalf("one ClusterRole expected: %d", len(docs))
	}
	namespacedWrites := map[string]bool{"secrets": true, "pods": true, "services": true, "serviceaccounts": true, "endpoints": true, "deployments": true,
		"daemonsets": true, "networkpolicies": true, "poddisruptionbudgets": true, "ciliumnetworkpolicies": true}
	for _, r := range rulesOf(t, docs[0]) {
		for _, res := range r.Resources {
			if res == "*" {
				t.Errorf("wildcard resource in %+v", r)
			}
			if res == "secrets" {
				t.Errorf("the cluster-wide role must not touch Secrets: %+v", r)
			}
			if namespacedWrites[res] {
				for _, v := range r.Verbs {
					if v != "get" && v != "list" && v != "watch" {
						t.Errorf("%s: cluster-wide verb %q: writes in a namespace come from the namespaced role", res, v)
					}
				}
			}
		}
		for _, v := range r.Verbs {
			if v == "*" || v == "escalate" || v == "impersonate" {
				t.Errorf("verb %q in %+v", v, r)
			}
		}
	}
	// It may only bind the roles it hands out.
	var bound []string
	for _, r := range rulesOf(t, docs[0]) {
		if has(r.Verbs, "bind") {
			bound = r.ResourceNames
		}
	}
	if len(bound) != 4 || !has(bound, "laboratory-proxy-reports") || !has(bound, "laboratory-operator-namespaced") || !has(bound, "laboratory-vpn-role") || !has(bound, "laboratory-agent-role") {
		t.Errorf("bind is limited to the named roles: %v", bound)
	}
}

func TestOperatorNamespacedRoleAndItsBindings(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/operator/clusterrole-namespaced.yaml", "-s", "templates/operator/rolebinding-namespaced.yaml", "-s", "templates/tenants-namespace.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"name: laboratory-operator-namespaced", "kind: RoleBinding", "name: laboratory-operator-tenants", "kind: Role\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "kind: ClusterRoleBinding\n  name: laboratory-operator-namespaced") {
		t.Error("the working role is never bound cluster-wide")
	}
}

// The prepull is an ImagePull request: the operator creates it cluster-wide, and no DaemonSet is made any more.
func TestOperatorMakesImagePullRequestsNotDaemonSets(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/operator/clusterrole.yaml", "-s", "templates/operator/clusterrole-namespaced.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	found := map[string]bool{}
	for _, d := range docs(t, out) {
		for _, r := range rulesOf(t, d) {
			if has(r.Resources, "daemonsets") {
				t.Errorf("the operator has no business with DaemonSets: %+v", r)
			}
			if has(r.Resources, "imagepulls") {
				for _, v := range r.Verbs {
					found[v] = true
				}
			}
		}
	}
	for _, v := range []string{"create", "delete", "get", "list", "watch"} {
		if !found[v] {
			t.Errorf("imagepulls: %s missing", v)
		}
	}
}

func TestOperatorAdmissionPolicy(t *testing.T) {
	render := func(extra ...string) (string, error) {
		return helmTemplate(t, append([]string{"-s", "templates/operator/admission-policy.yaml"}, extra...)...)
	}
	out, err := render("--kube-version", "1.33.0")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"name: laboratory-operator-scope", "name: laboratory-operator-namespaces", "name: laboratory-operator-pods",
		`system:serviceaccount:laboratory-system:laboratory-controller-manager`, "validationActions: [Deny]", "laboratory.cybericebox.com/group"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(out, "kind: ValidatingAdmissionPolicyBinding"); n != 3 {
		t.Errorf("bindings = %d, want 3", n)
	}
	// off by a value, and not rendered where the API does not exist
	if o, err := render("--kube-version", "1.33.0", "--set", "operator.admissionPolicy.enabled=false"); err == nil && strings.Contains(o, "ValidatingAdmissionPolicy") {
		t.Errorf("a disabled policy must not render:\n%s", o)
	}
	if o, err := render("--kube-version", "1.29.0"); err == nil && strings.Contains(o, "ValidatingAdmissionPolicy") {
		t.Errorf("Kubernetes 1.29 has no ValidatingAdmissionPolicy:\n%s", o)
	}
}

// The node-agent answers the requests, and reads Secrets in the images namespace only: its role reaches no platform Secret.
func TestNodeAgentReadsOnlyTheImagesNamespaceSecrets(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/node-agent/clusterrole.yaml", "-s", "templates/images-namespace.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		switch {
		case d["kind"] == "ClusterRole":
			var pulls, statusWrite bool
			for _, r := range rulesOf(t, d) {
				if has(r.Resources, "secrets") {
					t.Errorf("the cluster-wide node-agent role must not touch Secrets: %+v", r)
				}
				pulls = pulls || (has(r.Resources, "imagepulls") && has(r.Verbs, "watch"))
				statusWrite = statusWrite || (has(r.Resources, "imagepulls/status") && has(r.Verbs, "patch"))
			}
			if !pulls || !statusWrite {
				t.Errorf("the node-agent must watch imagepulls and write their status: %v %v", pulls, statusWrite)
			}
		case d["kind"] == "Role" && md["name"] == "laboratory-node-agent-images":
			if md["namespace"] != "laboratory-images" {
				t.Errorf("namespace %v", md["namespace"])
			}
			rs := rulesOf(t, d)
			if len(rs) != 1 || len(rs[0].Verbs) != 1 || rs[0].Verbs[0] != "get" || !has(rs[0].Resources, "secrets") {
				t.Errorf("the node-agent may only get Secrets there: %+v", rs)
			}
		}
	}
	if !strings.Contains(out, "name: laboratory-images") || !strings.Contains(out, "name: laboratory-operator-images") {
		t.Error("the images namespace and the operator's role in it")
	}
}

// A-3: the agent's Monitoring caches need list+watch of the namespaced kinds and of pods cluster-wide; that cluster-wide reach is read-only
// (writes stay in the group namespaces, through the role the operator binds there).
func TestAgentMonitoringCachesAreReadOnlyClusterWide(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/clusterrole.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "ClusterRole" || d["metadata"].(map[string]any)["name"] != "laboratory-agent-cluster" {
			continue
		}
		covered := map[string]bool{}
		for _, r := range rulesOf(t, d) {
			for _, res := range r.Resources {
				if has(r.APIGroups, "laboratory.cybericebox.com") && has([]string{"labs", "labgroupclients", "labgroupaccesspolicies", "labtrafficreports", "devices"}, res) {
					covered[res] = has(r.Verbs, "list") && has(r.Verbs, "watch") && !has(r.Verbs, "create") && !has(r.Verbs, "update") && !has(r.Verbs, "patch") && !has(r.Verbs, "delete")
				}
				if res == "pods" && has(r.APIGroups, "") && (!has(r.Verbs, "list") || !has(r.Verbs, "watch")) {
					t.Errorf("pods need list and watch for the informer: %+v", r)
				}
			}
		}
		for _, res := range []string{"labs", "labgroupclients", "labgroupaccesspolicies", "labtrafficreports", "devices"} {
			if !covered[res] {
				t.Errorf("%s must be readable (list, watch) cluster-wide and nothing more: %v", res, covered)
			}
		}
		return
	}
	t.Fatal("no laboratory-agent-cluster ClusterRole")
}
