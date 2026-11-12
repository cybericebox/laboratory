package laboratory

import (
	"context"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func checkedLab(name string, checks bool) *laboratoryv1alpha1.Lab {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g", UID: types.UID("uid-" + name)}}
	if checks {
		lab.Spec.Checks = &laboratoryv1alpha1.LabChecksSpec{Items: []laboratoryv1alpha1.CheckRef{{ID: "c", Device: "web"}}}
	}
	return lab
}

// The checker's right to read a Secret is one Role per lab with checks: get, that lab's checks Secret and no other name, a RoleBinding to
// the checker's ServiceAccount, both owned by the Lab. Nothing for a lab without checks or while the checker is off.
func TestCheckerSecretAccessIsOneRolePerLab(t *testing.T) {
	ctx := context.Background()
	s := systemScheme(t)
	_ = rbacv1.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	on := &LabReconciler{Client: c, Scheme: s, CheckerEnabled: true}
	exists := func(lab string) (bool, bool) {
		key := types.NamespacedName{Namespace: "g", Name: names.CheckerLabRoleName(lab)}
		return c.Get(ctx, key, &rbacv1.Role{}) == nil, c.Get(ctx, key, &rbacv1.RoleBinding{}) == nil
	}

	withChecks := checkedLab("one", true)
	for i := 0; i < 2; i++ { // idempotent; also what an existing lab gets on the first reconcile after an upgrade
		if err := on.ensureCheckerSecretAccess(ctx, withChecks); err != nil {
			t.Fatal(err)
		}
	}
	var role rbacv1.Role
	var rb rbacv1.RoleBinding
	key := types.NamespacedName{Namespace: "g", Name: names.CheckerLabRoleName("one")}
	if err := c.Get(ctx, key, &role); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &rb); err != nil {
		t.Fatal(err)
	}
	wantRules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"one-checks"}, Verbs: []string{"get"}}}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("rules %+v, want only get of one-checks", role.Rules)
	}
	if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != key.Name || len(rb.Subjects) != 1 ||
		rb.Subjects[0].Name != names.CheckerServiceAccount || rb.Subjects[0].Namespace != names.SystemNamespace {
		t.Fatalf("binding %+v", rb)
	}
	for _, owners := range [][]metav1.OwnerReference{role.OwnerReferences, rb.OwnerReferences} {
		if len(owners) != 1 || owners[0].Kind != "Lab" || owners[0].UID != withChecks.UID {
			t.Fatalf("owner references %+v, want the Lab", owners)
		}
	}

	// a lab without checks gets nothing (no API call: the client of this test has no Role kind to answer one)
	if err := on.ensureCheckerSecretAccess(ctx, checkedLab("two", false)); err != nil {
		t.Fatal(err)
	}
	if r, b := exists("two"); r || b {
		t.Fatal("a lab without checks has a Role")
	}
	// checks that were there and are now an empty list take the Role with them
	withChecks.Spec.Checks = &laboratoryv1alpha1.LabChecksSpec{}
	if err := on.ensureCheckerSecretAccess(ctx, withChecks); err != nil {
		t.Fatal(err)
	}
	if r, b := exists("one"); r || b {
		t.Fatal("the Role stayed after the checks were removed")
	}

	// the checker is off: nothing is made
	off := &LabReconciler{Client: c, Scheme: s}
	if err := off.ensureCheckerSecretAccess(ctx, checkedLab("three", true)); err != nil {
		t.Fatal(err)
	}
	if r, b := exists("three"); r || b {
		t.Fatal("a Role was made with the checker off")
	}
}
