package chart_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// The node-agent may set, on its own node, the ready label and the annotation of its start-up check of the runtime observation (and
// nothing else): the same patch that sets the label carries the check's result, and the API server must let it through. The annotation of
// state persistence is node-state's (TestNodeStateAnnotationAdmission).
func TestNodeAgentLabelAdmissionAllowsTheCheckAnnotations(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/node-agent/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir()}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start admission API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		var obj unstructured.Unstructured
		if yaml.Unmarshal([]byte(doc), &obj.Object) != nil || obj.Object == nil {
			continue
		}
		if err := admin.Create(ctx, &obj); err != nil {
			t.Fatal(err)
		}
	}
	const user = "system:serviceaccount:laboratory-system:laboratory-node-agent"
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "label-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
		t.Fatal(err)
	}
	imp := rest.CopyConfig(cfg)
	imp.Impersonate = rest.ImpersonationConfig{UserName: user, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"node-a"}}}
	agent, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"node-a", "node-b"} {
		if err := admin.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, Annotations: map[string]string{"keep": "me"}}}); err != nil {
			t.Fatal(err)
		}
	}
	const label, persistence, observation = "laboratory.cybericebox.com/node-agent-ready", "cybericebox.com/state-persistence", "cybericebox.com/runtime-observation"
	patch := func(node string, body string) error {
		return agent.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	// wait until the policy is enforced
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := patch("node-b", `{"metadata":{"labels":{"`+label+`":"true"}}}`); apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the policy is not enforced")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range []struct {
		name, node, body string
		ok               bool
	}{
		{"label and check annotation in one patch", "node-a", `{"metadata":{"labels":{"` + label + `":"true"},"annotations":{"` + observation + `":"unavailable: CGROUP_V2_MISSING: no cgroup v2 here"}}}`, true},
		{"the annotation again with another value", "node-a", `{"metadata":{"annotations":{"` + observation + `":"disabled"}}}`, true},
		{"the console relay annotation", "node-a", `{"metadata":{"annotations":{"cybericebox.com/console-relay":"ready"}}}`, true},
		{"the console relay annotation, unavailable", "node-a", `{"metadata":{"annotations":{"cybericebox.com/console-relay":"unavailable: CONSOLE_RELAY_UNREACHABLE: no answer"}}}`, true},
		{"the console relay key", "node-a", `{"metadata":{"annotations":{"laboratory.cybericebox.com/console-relay-key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}}`, true},
		{"a console relay key that is no key", "node-a", `{"metadata":{"annotations":{"laboratory.cybericebox.com/console-relay-key":"not a key"}}}`, false},
		{"the console relay key of another node", "node-b", `{"metadata":{"annotations":{"laboratory.cybericebox.com/console-relay-key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}}`, false},
		{"the console relay annotation of another node", "node-b", `{"metadata":{"annotations":{"cybericebox.com/console-relay":"ready"}}}`, false},
		{"the state persistence annotation is node-state's", "node-a", `{"metadata":{"annotations":{"` + persistence + `":"ready"}}}`, false},
		{"another node", "node-b", `{"metadata":{"annotations":{"` + observation + `":"ready"}}}`, false},
		{"another annotation", "node-a", `{"metadata":{"annotations":{"evil":"x"}}}`, false},
		{"remove a foreign annotation", "node-a", `{"metadata":{"annotations":{"keep":null}}}`, false},
		{"a value that is not a check result", "node-a", `{"metadata":{"annotations":{"` + observation + `":"anything you like"}}}`, false},
		{"a check result with no code", "node-a", `{"metadata":{"annotations":{"` + observation + `":"unavailable: no code"}}}`, false},
		{"a finalizer", "node-a", `{"metadata":{"finalizers":["example.com/evil"]}}`, false},
		{"another label", "node-a", `{"metadata":{"labels":{"evil":"x"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := patch(tc.node, tc.body)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("must be allowed: %v", err)
			case !tc.ok && !apierrors.IsForbidden(err):
				t.Fatalf("must be forbidden: %v", err)
			}
		})
	}
}

// node-state may set, on its own node, the annotation of its start-up check of state persistence and nothing else.
func TestNodeStateAnnotationAdmission(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/node-state/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir()}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start admission API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		var obj unstructured.Unstructured
		if yaml.Unmarshal([]byte(doc), &obj.Object) != nil || obj.Object == nil {
			continue
		}
		if err := admin.Create(ctx, &obj); err != nil {
			t.Fatal(err)
		}
	}
	const user = "system:serviceaccount:laboratory-system:laboratory-node-state"
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "state-annotation-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
		t.Fatal(err)
	}
	imp := rest.CopyConfig(cfg)
	imp.Impersonate = rest.ImpersonationConfig{UserName: user, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"node-a"}}}
	state, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"node-a", "node-b"} {
		if err := admin.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, Annotations: map[string]string{"keep": "me"}}}); err != nil {
			t.Fatal(err)
		}
	}
	const persistence, observation = "cybericebox.com/state-persistence", "cybericebox.com/runtime-observation"
	patch := func(node string, body string) error {
		return state.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := patch("node-b", `{"metadata":{"annotations":{"`+persistence+`":"ready"}}}`); apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the policy is not enforced")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range []struct {
		name, node, body string
		ok               bool
	}{
		{"its check result", "node-a", `{"metadata":{"annotations":{"` + persistence + `":"unavailable: SNAPSHOT_REGISTRY_UNREACHABLE: the registry is down"}}}`, true},
		{"the result again with another value", "node-a", `{"metadata":{"annotations":{"` + persistence + `":"ready"}}}`, true},
		{"another node", "node-b", `{"metadata":{"annotations":{"` + persistence + `":"ready"}}}`, false},
		{"the annotation of the runtime observation is the node-agent's", "node-a", `{"metadata":{"annotations":{"` + observation + `":"ready"}}}`, false},
		{"another annotation", "node-a", `{"metadata":{"annotations":{"evil":"x"}}}`, false},
		{"remove a foreign annotation", "node-a", `{"metadata":{"annotations":{"keep":null}}}`, false},
		{"a value that is not a check result", "node-a", `{"metadata":{"annotations":{"` + persistence + `":"anything you like"}}}`, false},
		{"a label", "node-a", `{"metadata":{"labels":{"laboratory.cybericebox.com/node-agent-ready":"true"}}}`, false},
		{"a finalizer", "node-a", `{"metadata":{"finalizers":["example.com/evil"]}}`, false},
		{"a taint", "node-a", `{"spec":{"taints":[{"key":"evil","effect":"NoSchedule"}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := patch(tc.node, tc.body)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("must be allowed: %v", err)
			case !tc.ok && !apierrors.IsForbidden(err):
				t.Fatalf("must be forbidden: %v", err)
			}
		})
	}
}

func envtestDir() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	dirs, _ := filepath.Glob("../../bin/k8s/*")
	if len(dirs) > 0 {
		return dirs[0]
	}
	return ""
}
