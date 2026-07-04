package laboratory

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe(
	"LabGroupReconciler default-deny NetworkPolicy", func() {
		It(
			"creates a default-deny NetworkPolicy when NetworkPolicyEnabled is true", func() {
				r := &LabGroupReconciler{
					Client:               k8sClient,
					Scheme:               k8sClient.Scheme(),
					NetworkPolicyEnabled: true,
				}

				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "grp-deny"}}
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, ns)
					},
				)

				Expect(r.ensureDefaultDeny(ctx, "grp-deny")).To(Succeed())

				var np networkingv1.NetworkPolicy
				Expect(
					k8sClient.Get(
						ctx,
						types.NamespacedName{Namespace: "grp-deny", Name: "default-deny"},
						&np,
					),
				).To(Succeed())

				Expect(np.Spec.PolicyTypes).To(HaveLen(2))
				Expect(np.Spec.PolicyTypes).To(ContainElements(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
				Expect(np.Spec.Ingress).To(BeEmpty())
				Expect(np.Spec.Egress).To(BeEmpty())
			},
		)

		It(
			"creates nothing when NetworkPolicyEnabled is false", func() {
				r := &LabGroupReconciler{
					Client:               k8sClient,
					Scheme:               k8sClient.Scheme(),
					NetworkPolicyEnabled: false,
				}

				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "grp-deny-disabled"}}
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, ns)
					},
				)

				Expect(r.ensureDefaultDeny(ctx, "grp-deny-disabled")).To(Succeed())

				var np networkingv1.NetworkPolicy
				err := k8sClient.Get(
					ctx,
					types.NamespacedName{Namespace: "grp-deny-disabled", Name: "default-deny"},
					&np,
				)
				Expect(err).To(HaveOccurred())
			},
		)
	},
)
