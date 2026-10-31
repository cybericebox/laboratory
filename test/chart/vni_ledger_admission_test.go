package chart_test

import (
	"context"
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

// The node-agent writes the VNI ledger ConfigMap of its own node and no other ConfigMap, with the label, the node annotation and
// numeric keys only.
func TestNodeAgentLedgerAdmission(t *testing.T) {
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
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ledger-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
		t.Fatal(err)
	}
	imp := rest.CopyConfig(cfg)
	imp.Impersonate = rest.ImpersonationConfig{UserName: user, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"node-a"}}}
	agent, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "laboratory-system"}}); err != nil {
		t.Fatal(err)
	}
	mk := func(name, node string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "laboratory-system",
			Labels: map[string]string{"laboratory.cybericebox.com/vni-ledger": "true"}, Annotations: map[string]string{"laboratory.cybericebox.com/node": node}}, Data: data}
	}
	// wait until the policy is enforced
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := agent.Create(ctx, mk("laboratory-bandwidth", "node-a", nil)); apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the policy is not enforced")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := agent.Create(ctx, mk("vni-ledger-node-a", "node-a", nil)); err != nil {
		t.Fatalf("its own ledger must be allowed: %v", err)
	}
	patch := func(name, body string) error {
		return agent.Patch(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "laboratory-system"}}, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	if err := patch("vni-ledger-node-a", `{"data":{"901":"2026-10-31T12:00:00Z"}}`); err != nil {
		t.Fatalf("a VNI entry must be allowed: %v", err)
	}
	if err := patch("vni-ledger-node-a", `{"data":{"901":null}}`); err != nil {
		t.Fatalf("taking an entry out must be allowed: %v", err)
	}
	for name, err := range map[string]error{
		"another node's ledger":      agent.Create(ctx, mk("vni-ledger-node-b", "node-b", nil)),
		"a foreign ConfigMap":        agent.Create(ctx, mk("anything", "node-a", nil)),
		"a ledger claiming a node":   agent.Create(ctx, mk("vni-ledger-node-a2", "node-a", nil)),
		"a key that is not a number": patch("vni-ledger-node-a", `{"data":{"evil":"x"}}`),
		"the label removed":          patch("vni-ledger-node-a", `{"metadata":{"labels":{"laboratory.cybericebox.com/vni-ledger":null}}}`),
		"the node annotation moved":  patch("vni-ledger-node-a", `{"metadata":{"annotations":{"laboratory.cybericebox.com/node":"node-b"}}}`),
	} {
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s must be forbidden: %v", name, err)
		}
	}
}
