package laboratory

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var _ = Describe("LabGroup suspension", func() {
	It("scales existing VPN and gateway deployments down", func() {
		const namespace = "default"
		for _, name := range []string{"vpn", "gateway"} {
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptrInt32(1),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: "test"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		}

		r := &LabGroupReconciler{Client: k8sClient, VPNBaseNetwork: "10.8.0.0/10"}
		Expect(r.ensureVPNDeployment(ctx, namespace, true)).To(Succeed())
		Expect(r.ensureGatewayDeployment(ctx, namespace, true)).To(Succeed())

		for _, name := range []string{"vpn", "gateway"} {
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(0)))
		}
	})
})

var _ = Describe(
	"LabGroup controller", func() {
		const (
			timeout  = 15 * time.Second
			interval = 250 * time.Millisecond
		)

		It(
			"creates namespace, VPN keypair, and pools on CREATE", func() {
				lg := &laboratoryv1alpha1.LabGroup{
					ObjectMeta: metav1.ObjectMeta{Name: "test-labgroup"},
				}
				Expect(k8sClient.Create(ctx, lg)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, lg)
					},
				)

				// Wait for the reconciler to populate Status.Namespace.
				Eventually(
					func() string {
						var updated laboratoryv1alpha1.LabGroup
						if err := k8sClient.Get(
							ctx,
							types.NamespacedName{Name: "test-labgroup"},
							&updated,
						); err != nil {
							return ""
						}
						return updated.Status.Namespace
					}, timeout, interval,
				).ShouldNot(BeEmpty())

				var updated laboratoryv1alpha1.LabGroup
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-labgroup"}, &updated)).To(Succeed())

				Expect(updated.Status.Phase).To(Equal(laboratoryv1alpha1.PhaseReady))
				Expect(updated.Status.VPN.PublicKey).NotTo(BeEmpty())

				// The namespace must exist in the cluster.
				var ns corev1.Namespace
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: updated.Status.Namespace}, &ns)).To(Succeed())
			},
		)
	},
)
