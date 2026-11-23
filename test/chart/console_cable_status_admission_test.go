package chart_test

import (
	"context"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// A node-agent may write the status of a ConsoleCable only when the operator's stamp names its node: not a cable of another node, not one
// without a stamp, and not by naming its own node in the status.
func TestNodeAgentConsoleCableStatusAdmission(t *testing.T) {
	out, err := helmTemplate(t, "--set", "consoleCables.enabled=true", "-s", "templates/node-agent/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir(), CRDDirectoryPaths: []string{"../../charts/laboratory/crds"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start admission API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = laboratoryv1alpha1.AddToScheme(sch)
	admin, err := client.New(cfg, client.Options{Scheme: sch})
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
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cable-status-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
		t.Fatal(err)
	}
	imp := rest.CopyConfig(cfg)
	imp.Impersonate = rest.ImpersonationConfig{UserName: user, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"node-a"}}}
	agent, err := client.New(imp, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	cable := func(name, stamp string) *laboratoryv1alpha1.ConsoleCable {
		c := &laboratoryv1alpha1.ConsoleCable{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: laboratoryv1alpha1.ConsoleCableSpec{LabRef: "l",
			Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}
		if stamp != "" {
			c.Annotations = map[string]string{names.AnnotationConsoleNode: stamp}
		}
		if err := admin.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	write := func(c *laboratoryv1alpha1.ConsoleCable) error {
		var cur laboratoryv1alpha1.ConsoleCable
		if err := agent.Get(ctx, client.ObjectKeyFromObject(c), &cur); err != nil {
			return err
		}
		cur.Status.LineUp = true
		cur.Status.Console.NodeName = "node-a"
		return agent.Status().Update(ctx, &cur)
	}
	mine, other, none := cable("mine", "node-a"), cable("other", "node-b"), cable("none", "")
	// wait until the policy is enforced
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := write(other); apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the policy is not enforced")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := write(mine); err != nil {
		t.Fatalf("the cable of its own node: %v", err)
	}
	if err := write(none); !apierrors.IsForbidden(err) {
		t.Fatalf("a cable without a stamp: %v", err)
	}
	// the operator is not bound by it
	var cur laboratoryv1alpha1.ConsoleCable
	_ = admin.Get(ctx, client.ObjectKeyFromObject(other), &cur)
	cur.Status.LineUp = true
	if err := admin.Status().Update(ctx, &cur); err != nil {
		t.Fatalf("another user: %v", err)
	}
}
