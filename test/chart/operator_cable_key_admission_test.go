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
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// With console cables on, the operator may create the cable key Secret in the device access namespace, and nothing else there: no other
// Secret, and no change of the key once it exists. With them off, not even that.
func TestOperatorScopeAdmitsOnlyTheCableKey(t *testing.T) {
	for _, on := range []bool{true, false} {
		args := []string{"-s", "templates/operator/admission-policy.yaml"}
		if on {
			args = append(args, "--set", "consoleCables.enabled=true")
		}
		out, err := helmTemplate(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		env := &envtest.Environment{BinaryAssetsDirectory: envtestDir()}
		cfg, err := env.Start()
		if err != nil {
			t.Fatalf("start admission API server: %v", err)
		}
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
		_ = admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "laboratory-device-access"}})
		const user = "system:serviceaccount:laboratory-system:laboratory-controller-manager"
		if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "scope-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
			t.Fatal(err)
		}
		imp := rest.CopyConfig(cfg)
		imp.Impersonate = rest.ImpersonationConfig{UserName: user}
		op, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			t.Fatal(err)
		}
		secret := func(name string) *corev1.Secret {
			return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "laboratory-device-access"}, StringData: map[string]string{"key": strings.Repeat("k", 64)}}
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			if err := op.Create(ctx, secret("other")); apierrors.IsForbidden(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the policy is not enforced")
			}
			time.Sleep(100 * time.Millisecond)
		}
		key := secret("laboratory-console-cable-key")
		err = op.Create(ctx, key)
		switch {
		case on && err != nil:
			t.Fatalf("console cables on, the key: %v", err)
		case !on && !apierrors.IsForbidden(err):
			t.Fatalf("console cables off, the key: %v", err)
		}
		if on {
			key.StringData = map[string]string{"key": "changed-by-the-operator-" + strings.Repeat("x", 40)}
			if err := op.Update(ctx, key); !apierrors.IsForbidden(err) {
				t.Fatalf("the operator changed the key: %v", err)
			}
		}
		_ = env.Stop()
	}
}
