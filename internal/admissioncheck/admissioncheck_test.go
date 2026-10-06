package admissioncheck

import (
	"context"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func policyObjects(deny bool) []client.Object {
	var objs []client.Object
	for _, n := range []string{PolicyScope, PolicyNamespaces, PolicyPods} {
		actions := []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}
		if !deny {
			actions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn}
		}
		objs = append(objs,
			&admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: n}},
			&admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: n}, Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: n, ValidationActions: actions}})
	}
	return objs
}

// denyingServer answers the dry-run probes like an API server with the policies enforced.
func denyingServer(objs []client.Object, enforce bool) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if !enforce {
				return nil
			}
			policy := PolicyScope
			if obj.GetObjectKind().GroupVersionKind().Kind == "Namespace" || strings.HasSuffix(obj.GetName(), "probe") && obj.GetNamespace() == "" {
				policy = PolicyNamespaces
			}
			return apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, obj.GetName(),
				&forbidden{"ValidatingAdmissionPolicy '" + policy + "' with binding '" + policy + "' denied request: nope"})
		},
	}).Build()
}

type forbidden struct{ msg string }

func (f *forbidden) Error() string { return f.msg }

func TestVerifyAcceptsAnEnforcedPolicy(t *testing.T) {
	if err := Verify(context.Background(), denyingServer(policyObjects(true), true)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefusesAClusterThatDoesNotEnforce(t *testing.T) {
	cases := map[string]struct {
		c    client.Client
		want string
	}{
		"no policy objects":        {denyingServer(nil, true), "does not exist"},
		"binding does not deny":    {denyingServer(policyObjects(false), true), "does not deny"},
		"objects but not enforced": {denyingServer(policyObjects(true), false), "not enforced"},
	}
	for name, c := range cases {
		err := Verify(context.Background(), c.c)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// a refusal that is not the policy's (for instance RBAC) is not taken for enforcement
	rbac := fake.NewClientBuilder().WithScheme(func() *runtime.Scheme { s := runtime.NewScheme(); _ = clientgoscheme.AddToScheme(s); return s }()).
		WithObjects(policyObjects(true)...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "rolebindings"}, obj.GetName(), &forbidden{"RBAC: rolebindings is forbidden"})
		},
	}).Build()
	if err := Verify(context.Background(), rbac); err == nil || strings.Contains(err.Error(), "not enforced") {
		t.Errorf("an RBAC refusal must be an error of its own, not a pass: %v", err)
	}
}
