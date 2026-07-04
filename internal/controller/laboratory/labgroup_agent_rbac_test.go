package laboratory

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe(
	"LabGroupReconciler agent RoleBinding", func() {
		It(
			"creates the agent RoleBinding when AgentEnabled is true", func() {
				r := &LabGroupReconciler{
					Client:       k8sClient,
					Scheme:       k8sClient.Scheme(),
					AgentEnabled: true,
					AgentSA: types.NamespacedName{
						Namespace: "laboratory-agent",
						Name:      "laboratory-agent",
					},
				}

				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "grp-agent-rbac"}}
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, ns)
					},
				)

				Expect(r.ensureAgentRoleBinding(ctx, "grp-agent-rbac")).To(Succeed())

				var rb rbacv1.RoleBinding
				Expect(
					k8sClient.Get(
						ctx,
						types.NamespacedName{Namespace: "grp-agent-rbac", Name: "laboratory-agent-binding"},
						&rb,
					),
				).To(Succeed())

				Expect(rb.RoleRef.Name).To(Equal("laboratory-agent-role"))
				Expect(rb.Subjects).To(HaveLen(1))
				Expect(rb.Subjects[0].Namespace).To(Equal("laboratory-agent"))
				Expect(rb.Subjects[0].Name).To(Equal("laboratory-agent"))
			},
		)

		It(
			"creates nothing when AgentEnabled is false", func() {
				r := &LabGroupReconciler{
					Client:       k8sClient,
					Scheme:       k8sClient.Scheme(),
					AgentEnabled: false,
					AgentSA: types.NamespacedName{
						Namespace: "laboratory-agent",
						Name:      "laboratory-agent",
					},
				}

				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "grp-agent-rbac-disabled"}}
				Expect(k8sClient.Create(ctx, ns)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, ns)
					},
				)

				Expect(r.ensureAgentRoleBinding(ctx, "grp-agent-rbac-disabled")).To(Succeed())

				var rb rbacv1.RoleBinding
				err := k8sClient.Get(
					ctx,
					types.NamespacedName{Namespace: "grp-agent-rbac-disabled", Name: "laboratory-agent-binding"},
					&rb,
				)
				Expect(errors.IsNotFound(err)).To(BeTrue())
			},
		)
	},
)
