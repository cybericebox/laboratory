// Package admissioncheck lets the operator verify, before it works, that the chart's admission policies are really enforced on
// its ServiceAccount. RBAC alone cannot confine the operator to the namespaces of its LabGroups (they do not exist when the chart is
// installed), so without the policies the operator could bind its role in any namespace and read what it holds there. A cluster that
// does not enforce them must not run the operator silently.
package admissioncheck

import (
	"context"
	"fmt"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
)

// The policies of the chart (templates/operator/admission-policy.yaml); each has a binding of the same name.
const (
	PolicyScope      = "laboratory-operator-scope"
	PolicyNamespaces = "laboratory-operator-namespaces"
	PolicyPods       = "laboratory-operator-pods"
)

const probeName = "laboratory-admission-probe"

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies;validatingadmissionpolicybindings,verbs=get

// Verify returns nil when the three policies exist with a binding that denies, and the operator is in fact refused outside the
// namespaces of its groups. It writes nothing: the probes are dry runs, which the API server admits or refuses exactly like a real
// request and then discards.
func Verify(ctx context.Context, c client.Client) error {
	for _, name := range []string{PolicyScope, PolicyNamespaces, PolicyPods} {
		var p admissionregistrationv1.ValidatingAdmissionPolicy
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &p); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("the admission policy %q does not exist: the chart installs it (Kubernetes 1.30 or newer)", name)
			}
			return fmt.Errorf("read the admission policy %q: %w", name, err)
		}
		var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &b); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("the admission policy %q has no binding, so it is not enforced", name)
			}
			return fmt.Errorf("read the binding of %q: %w", name, err)
		}
		deny := false
		for _, a := range b.Spec.ValidationActions {
			deny = deny || a == admissionregistrationv1.Deny
		}
		if b.Spec.PolicyName != name || !deny {
			return fmt.Errorf("the binding of the admission policy %q does not deny", name)
		}
	}

	// The policies exist; are they enforced? Ask for what they forbid. A RoleBinding in a namespace that is no group's, and a
	// namespace that is no group's, must be refused by the policies themselves (the message names the policy), never allowed.
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: probeName, Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: names.RoleOperatorNamespacedName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: probeName, Namespace: "default"}},
	}
	if err := refused(c.Create(ctx, rb, client.DryRunAll), PolicyScope, "a RoleBinding in the namespace default"); err != nil {
		return err
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeName}}
	return refused(c.Create(ctx, ns, client.DryRunAll), PolicyNamespaces, "a namespace that is no group's")
}

// refused checks that a dry-run request ended in the denial of the named policy.
func refused(err error, policy, what string) error {
	switch {
	case err == nil:
		return fmt.Errorf("the operator could create %s: the admission policy %q is not enforced", what, policy)
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+policy+"'"):
		return nil
	default:
		return fmt.Errorf("probing the admission policy %q with %s: %w", policy, what, err)
	}
}

// Wait runs Verify until it succeeds or the time is up: the policies are applied in the same release as the operator and need a
// moment to be compiled and enforced.
func Wait(ctx context.Context, c client.Client, timeout, every time.Duration, log func(error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		if last = Verify(ctx, c); last == nil {
			return nil
		}
		log(last)
		select {
		case <-ctx.Done():
			return last
		case <-time.After(every):
		}
	}
}
