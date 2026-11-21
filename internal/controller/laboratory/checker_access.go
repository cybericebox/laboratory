package laboratory

import (
	"context"
	"slices"
	"sort"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
)

// The lab checker reads one Secret of a lab: `<lab>-checks`, the document of its checks. Its right to do so is a Role in the group
// namespace that names that Secret (get, resourceNames) and a RoleBinding to the checker's ServiceAccount, both owned by the Lab, so they
// go with it. A lab without checks, or a laboratory without the checker, gets neither. The group namespace holds more Secrets (the
// environment and bootstrap of the devices, the VPN keys, the pull secrets) that the checker never needs; a namespace-wide right would
// give it all of them, for the labs of every group.
//
// The Role is made when the Lab is first reconciled (and for every existing lab on the first reconcile after an upgrade). A checker that
// asks before it exists is refused and asks again (its read of the Secret is retried).

// ensureCheckerSecretAccess makes, keeps or removes the Role and the RoleBinding of the lab.
func (r *LabReconciler) ensureCheckerSecretAccess(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	// A lab that never had checks, or any lab while the checker is off, costs no API call: a Role that an earlier run of the checker left
	// stays harmless (nothing runs under that ServiceAccount) and goes with the Lab.
	if !r.CheckerEnabled || lab.Spec.Checks == nil || !lab.DeletionTimestamp.IsZero() {
		return nil
	}
	name := names.CheckerLabRoleName(lab.Name)
	key := types.NamespacedName{Namespace: lab.Namespace, Name: name}
	if len(lab.Spec.Checks.Items) == 0 {
		for _, obj := range []client.Object{&rbacv1.RoleBinding{}, &rbacv1.Role{}} {
			obj.SetName(name)
			obj.SetNamespace(lab.Namespace)
			if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: []string{lab.Name + labchecks.ChecksSecretSuffix},
			Verbs:         []string{"get"},
		}}
		return controllerutil.SetControllerReference(lab, role, r.Scheme)
	}); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		// roleRef is immutable, and it never changes: the name is derived from the lab.
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
		binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: names.CheckerServiceAccount, Namespace: names.SystemNamespace}}
		return controllerutil.SetControllerReference(lab, binding, r.Scheme)
	})
	return err
}

// nodesOfDevices is the sorted, distinct set of the nodes the devices run on at this moment (Lab.status.deviceNodes). A device that
// moves is simply recomputed: its new node is listed as soon as the Device reports it, and the old one is dropped when no other device
// of the lab is there. The lab checker of a node may write the results of a lab only while its node is in this set; a checker whose
// node was just added is refused for a moment and writes again (its status write is retried).
func nodesOfDevices(devices []laboratoryv1alpha1.Device) []string {
	var out []string
	for i := range devices {
		if n := devices[i].Status.NodeName; n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
