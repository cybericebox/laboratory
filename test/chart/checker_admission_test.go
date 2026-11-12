package chart_test

import (
	"context"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
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

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The checker's admission policies against a real API server: it may write status.checks and status.checksRunAck of a Lab and nothing
// else of it, and the Lease of its own node and no other.
func TestCheckerAdmissionPolicies(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, checksOn...), "-s", "templates/checker/admission-policy.yaml")...)
	if err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir(), CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
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
	const user = "system:serviceaccount:laboratory-system:laboratory-checker"
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "checker-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
		t.Fatal(err)
	}
	as := func(node string) client.Client {
		imp := rest.CopyConfig(cfg)
		imp.Impersonate = rest.ImpersonationConfig{UserName: user}
		if node != "" {
			imp.Impersonate.Extra = map[string][]string{"authentication.kubernetes.io/node-name": {node}}
		}
		c, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, ns := range []string{"laboratory-system", "labs"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "labs", Annotations: map[string]string{"keep": "me"}}}
	lab.Spec.Checks = &laboratoryv1alpha1.LabChecksSpec{Items: []laboratoryv1alpha1.CheckRef{{ID: "a", Device: "web"}}}
	if err := admin.Create(ctx, lab); err != nil {
		t.Fatal(err)
	}
	// The operator records where the devices of a lab run (here as the admin: the policy matches the checker only).
	other := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "labs"}}
	if err := admin.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	patchLab := func(c client.Client, name, body string) error {
		return c.Status().Patch(ctx, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "labs"}}, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	if err := patchLab(admin, "lab", `{"status":{"deviceNodes":["node-a","node-c"]}}`); err != nil {
		t.Fatal(err)
	}
	mergeStatus := func(c client.Client, body string) error { return patchLab(c, "lab", body) }
	// wait until the policy is enforced
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := mergeStatus(as("node-a"), `{"status":{"phase":"Ready"}}`)
		if apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy is not enforced: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range []struct {
		name string
		node string
		body string
		ok   bool
	}{
		{"a check result", "node-a", `{"status":{"checks":[{"id":"a","state":"passed","firstPassedMs":1,"changedMs":1}]}}`, true},
		{"the acknowledgement", "node-a", `{"status":{"checksRunAck":3}}`, true},
		{"the phase", "node-a", `{"status":{"phase":"Failed"}}`, false},
		{"the devices", "node-a", `{"status":{"devices":[{"name":"web","ready":true}]}}`, false},
		{"the conditions", "node-a", `{"status":{"conditions":[{"type":"Ready","status":"True","reason":"x","message":"x","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`, false},
		{"a result together with the phase", "node-a", `{"status":{"phase":"Failed","checks":[{"id":"a","state":"error"}]}}`, false},
		{"no node in the token", "", `{"status":{"checksRunAck":5}}`, false},
		{"a result from a node of the lab that is not the first", "node-c", `{"status":{"checksRunAck":4}}`, true},
		{"a result from a node that runs no device of the lab", "node-b", `{"status":{"checks":[{"id":"a","state":"passed","firstPassedMs":1,"changedMs":1}]}}`, false},
		{"an acknowledgement from a node that runs no device of the lab", "node-b", `{"status":{"checksRunAck":9}}`, false},
		{"the list of nodes itself", "node-a", `{"status":{"deviceNodes":["node-a","node-b"]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := mergeStatus(as(tc.node), tc.body)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("must be allowed: %v", err)
			case !tc.ok && !apierrors.IsForbidden(err):
				t.Fatalf("must be forbidden: %v", err)
			}
		})
	}
	// a lab whose nodes are not recorded (yet) accepts nothing from any checker
	if err := patchLab(as("node-a"), "other", `{"status":{"checksRunAck":1}}`); !apierrors.IsForbidden(err) {
		t.Fatalf("a lab with no recorded nodes must be forbidden: %v", err)
	}
	lease := func(name string) *coordinationv1.Lease {
		return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "laboratory-system"}}
	}
	if err := as("node-a").Create(ctx, lease("laboratory-checker-node-a")); err != nil {
		t.Fatalf("the Lease of its own node: %v", err)
	}
	if err := as("node-a").Create(ctx, lease("laboratory-checker-node-b")); !apierrors.IsForbidden(err) {
		t.Fatalf("the Lease of another node must be forbidden: %v", err)
	}
	if err := as("").Create(ctx, lease("laboratory-checker-x")); !apierrors.IsForbidden(err) {
		t.Fatalf("a token with no node must be forbidden: %v", err)
	}
	// The metadata of a Lab is out of reach through the status endpoint whatever the policy says (the API server drops it); the policy
	// keeps the status to the checker's two fields.
	if err := mergeStatus(as("node-a"), `{"metadata":{"annotations":{"evil":"x"},"labels":{"evil":"x"}},"status":{"checksRunAck":3}}`); err != nil {
		t.Fatal(err)
	}
	var got laboratoryv1alpha1.Lab
	if err := admin.Get(ctx, client.ObjectKeyFromObject(lab), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "" || got.Status.ChecksRunAck != 3 || len(got.Status.Checks) != 1 || got.Annotations["keep"] != "me" || len(got.Labels) != 0 {
		t.Fatalf("what the policy refused leaked into the object: %+v", got)
	}
}
