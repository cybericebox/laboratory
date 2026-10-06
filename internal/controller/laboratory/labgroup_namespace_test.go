package laboratory

import (
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// The namespace of a new group is prefixed and hashed: a tenant-chosen name can never equal an existing namespace.
func TestGroupNamespaceNeverEqualsAGroupName(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"kube-system", "kube-public", "kube-node-lease", "default", "laboratory-system", "laboratory-tenants", "laboratory-proxy",
		"laboratory-images", "lg-kube-system", strings.Repeat("a", 63), strings.Repeat("a", 62) + "b", "team-1", "e-01k-t-01k"} {
		ns := laboratoryv1alpha1.LabGroupNamespace(name)
		if !strings.HasPrefix(ns, "lg-") || ns == name {
			t.Errorf("%s -> %s: must carry the prefix", name, ns)
		}
		if len(ns) > 56 {
			t.Errorf("%s -> %s: %d characters", name, ns, len(ns))
		}
		if strings.HasSuffix(ns, "--") || strings.Contains(ns, "--") && !strings.Contains(name, "--") {
			t.Errorf("%s -> %s: odd name", name, ns)
		}
		if other, dup := seen[ns]; dup {
			t.Errorf("%s and %s share the namespace %s", name, other, ns)
		}
		seen[ns] = name
	}
	if laboratoryv1alpha1.LabGroupNamespace("team-1") != laboratoryv1alpha1.LabGroupNamespace("team-1") {
		t.Error("the name is deterministic")
	}
	// two long names with the same first 40 characters still differ (the hash covers the whole name)
	a, b := strings.Repeat("a", 50), strings.Repeat("a", 49)+"b"
	if laboratoryv1alpha1.LabGroupNamespace(a) == laboratoryv1alpha1.LabGroupNamespace(b) {
		t.Error("the hash covers the whole name")
	}
	// a group created under the old naming keeps the namespace in its status
	old := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "legacy"}}
	if got := laboratoryv1alpha1.LabGroupNamespaceOf(old); got != "legacy" {
		t.Errorf("an old group keeps its namespace: %s", got)
	}
	if got := laboratoryv1alpha1.LabGroupNamespaceOf(&laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "fresh"}}); got != laboratoryv1alpha1.LabGroupNamespace("fresh") {
		t.Errorf("a new group gets the prefixed one: %s", got)
	}
}

var _ = Describe("LabGroup namespaces", func() {
	exists := func(name string) bool {
		var ns corev1.Namespace
		return k8sClient.Get(ctx, types.NamespacedName{Name: name}, &ns) == nil
	}

	// The attack of the audit: a tenant names a group after an existing namespace. (The CRD refuses the names of the system
	// namespaces outright; any other existing namespace, like these two, is what the prefix protects.) The operator must
	// provision, and later delete, only its own prefixed namespace.
	It("never touches the namespace a group name equals", func() {
		for _, victim := range []string{"victim-system", "victim-tenants"} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: victim}})).To(Succeed())
			group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: victim}}
			Expect(k8sClient.Create(ctx, group)).To(Succeed())
			own := laboratoryv1alpha1.LabGroupNamespace(victim)
			Eventually(func(g Gomega) {
				var lg laboratoryv1alpha1.LabGroup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: victim}, &lg)).To(Succeed())
				g.Expect(lg.Status.Namespace).To(Equal(own))
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: own}, &dep)).To(Succeed())
			}, 20*time.Second, 250*time.Millisecond).Should(Succeed())

			// nothing was provisioned in the victim namespace
			var deps appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &deps, client.InNamespace(victim))).To(Succeed())
			Expect(deps.Items).To(BeEmpty(), "no VPN or gateway Deployment in %s", victim)
			var pdbs policyv1.PodDisruptionBudgetList
			Expect(k8sClient.List(ctx, &pdbs, client.InNamespace(victim))).To(Succeed())
			Expect(pdbs.Items).To(BeEmpty())

			// deleting the group removes its own namespace and leaves the victim
			Expect(k8sClient.Delete(ctx, group)).To(Succeed())
			Eventually(func() bool {
				var ns corev1.Namespace
				err := k8sClient.Get(ctx, types.NamespacedName{Name: own}, &ns)
				return apierrors.IsNotFound(err) || (err == nil && !ns.DeletionTimestamp.IsZero())
			}, 30*time.Second, 250*time.Millisecond).Should(BeTrue())
			// (envtest has no namespace controller, so the namespace stays Terminating and the group waits for it)
			Expect(exists(victim)).To(BeTrue(), "the victim namespace %s must survive", victim)
			var ns corev1.Namespace
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: victim}, &ns)).To(Succeed())
			Expect(ns.DeletionTimestamp.IsZero()).To(BeTrue())
		}
	})

	// A namespace that already exists under the name a group would get, without the group's label, is never adopted
	// and never deleted.
	It("refuses to adopt or delete a namespace without its label", func() {
		const name = "adopt-me"
		foreign := laboratoryv1alpha1.LabGroupNamespace(name)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: foreign}})).To(Succeed())
		group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		Consistently(func() int {
			var deps appsv1.DeploymentList
			_ = k8sClient.List(ctx, &deps, client.InNamespace(foreign))
			return len(deps.Items)
		}, 3*time.Second, 250*time.Millisecond).Should(BeZero(), "nothing may be provisioned in a foreign namespace")
		var ns corev1.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: foreign}, &ns)).To(Succeed())
		Expect(ns.Labels).NotTo(HaveKey(names.LabelGroup), "the foreign namespace is not labelled either")

		Expect(k8sClient.Delete(ctx, group)).To(Succeed())
		Eventually(func() bool {
			var lg laboratoryv1alpha1.LabGroup
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &lg))
		}, 30*time.Second, 250*time.Millisecond).Should(BeTrue(), "the group is let go")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: foreign}, &ns)).To(Succeed())
		Expect(ns.DeletionTimestamp.IsZero()).To(BeTrue(), "the foreign namespace must not be deleted")
	})
})

// L-1: two long ids that share their first 40 characters differ in the hash (48 bits), and a namespace stays inside the 63 of a name.
func TestGroupNamespaceHashIsTwelveDigits(t *testing.T) {
	a := laboratoryv1alpha1.LabGroupNamespace(strings.Repeat("a", 40) + "-1")
	b := laboratoryv1alpha1.LabGroupNamespace(strings.Repeat("a", 40) + "-2")
	if a == b {
		t.Fatal("long ids with a common prefix collide")
	}
	if len(a) != 3+40+1+12 || len(a) > 63 {
		t.Fatalf("%s: %d characters", a, len(a))
	}
	// a group that exists keeps the namespace in its status, whatever its length
	lg := &laboratoryv1alpha1.LabGroup{}
	lg.Name, lg.Status.Namespace = "old", "lg-old-1a2b3c4d"
	if got := laboratoryv1alpha1.LabGroupNamespaceOf(lg); got != "lg-old-1a2b3c4d" {
		t.Fatalf("an existing namespace is kept: %s", got)
	}
}
