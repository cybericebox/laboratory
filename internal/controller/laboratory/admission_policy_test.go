package laboratory

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/cybericebox/laboratory/internal/names"
)

// The chart's ValidatingAdmissionPolicy objects, rendered by helm and applied to a real API server: the operator's
// ServiceAccount (impersonated, with cluster-admin RBAC so that only the admission policy limits it) may work in the
// namespaces of its LabGroups and nowhere else.
var _ = Describe("operator admission policy", Ordered, func() {
	const operator = "system:serviceaccount:laboratory-system:laboratory-controller-manager"
	var op client.Client

	BeforeAll(func() {
		if _, err := exec.LookPath("helm"); err != nil {
			Skip("helm is not installed")
		}
		out, err := exec.Command("helm", "template", "x", "../../../charts/laboratory", "--namespace", "laboratory-system", "--kube-version", "1.33.0",
			"--set", "operator.baseDomain=lab.example.com", "--set", "operator.publicVPNEndpoint=vpn.example.com:51820", "--set", "operator.supportEmail=a@example.com",
			"-s", "templates/operator/admission-policy.yaml").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		applied := 0
		for _, doc := range strings.Split(string(out), "\n---\n") {
			var obj unstructured.Unstructured
			if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil || obj.Object == nil {
				continue
			}
			Expect(k8sClient.Create(ctx, &obj)).To(Succeed(), doc)
			applied++
		}
		Expect(applied).To(Equal(6), "three policies and their bindings")

		// Only the admission policy may stand between the operator and the cluster in this test.
		Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "adm-test-operator"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: operator}},
		})).To(Succeed())
		impersonating := rest.CopyConfig(cfg)
		impersonating.Impersonate = rest.ImpersonationConfig{UserName: operator}
		op, err = client.New(impersonating, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		// The policies take a moment to be compiled and enforced.
		Eventually(func() error {
			err := op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"}})
			if err == nil {
				_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"}})
				return fmt.Errorf("created")
			}
			if !apierrors.IsForbidden(err) {
				return err
			}
			return nil
		}, 30*time.Second, 500*time.Millisecond).Should(Succeed(), "the policy must start denying")
	})

	groupNamespace := func(name string) {
		GinkgoHelper()
		Expect(op.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{names.LabelGroup: name}}})).To(Succeed())
	}
	denied := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), err.Error())
	}

	It("lets the operator create the namespace of a LabGroup and write in it", func() {
		groupNamespace("adm-group")
		Expect(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "adm-group"}})).To(Succeed())
		Expect(op.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "adm-group"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: "adm-group"}},
		})).To(Succeed())
		Expect(op.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "adm-group"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}}})).To(Succeed())
	})

	It("refuses a namespace that is not a LabGroup's, and any change of one that is not the operator's", func() {
		denied(op.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "adm-not-a-group"}}))
		var kube corev1.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kube-system"}, &kube)).To(Succeed())
		patch := client.MergeFrom(kube.DeepCopy())
		kube.Labels = map[string]string{"laboratory.cybericebox.com/group": "x"}
		denied(op.Patch(ctx, &kube, patch))
		denied(op.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
	})

	It("refuses writes outside the namespaces of its groups, and RoleBindings anywhere but there", func() {
		denied(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}))
		denied(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "kube-system"}}))
		// the release namespace is allowed for what the operator does there, but not for RoleBindings
		Expect(op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adm-copy", Namespace: "laboratory-system"}})).To(Succeed())
		denied(op.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "rb", Namespace: "laboratory-system"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "default", Namespace: "laboratory-system"}},
		}))
		// someone else is not limited by these policies
		Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adm-admin", Namespace: "default"}})).To(Succeed())
	})

	It("refuses pods that reach into the node", func() {
		groupNamespace("adm-pods")
		yes := true
		priv := &corev1.SecurityContext{Privileged: &yes}
		for name, spec := range map[string]corev1.PodSpec{
			"host-network": {HostNetwork: true, Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			"host-pid":     {HostPID: true, Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			"privileged":   {Containers: []corev1.Container{{Name: "c", Image: "x", SecurityContext: priv}}},
			"privileged-init": {InitContainers: []corev1.Container{{Name: "i", Image: "x", SecurityContext: priv}},
				Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			"host-path": {Containers: []corev1.Container{{Name: "c", Image: "x"}},
				Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}},
		} {
			denied(op.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "adm-pods"}, Spec: spec}))
		}
	})
})
