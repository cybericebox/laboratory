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
	It("keeps the tunnel and internet gateway available while Lab devices are suspended", func() {
		const name = "probe-while-suspended"
		group := &laboratoryv1alpha1.LabGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: laboratoryv1alpha1.LabGroupSpec{
				Suspended: true,
			},
		}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, group) })
		Eventually(func() bool {
			var current laboratoryv1alpha1.LabGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil || current.Status.Phase != laboratoryv1alpha1.PhaseSuspended {
				return false
			}
			var vpn, gateway appsv1.Deployment
			var vpnService corev1.Service
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: name}, &vpnService); err != nil || len(vpnService.Spec.Ports) != 1 || vpnService.Spec.Ports[0].Protocol != corev1.ProtocolUDP || vpnService.Spec.Ports[0].Port != 51820 {
				return false
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: name}, &vpn); err != nil {
				return false
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: name}, &gateway); err != nil {
				return false
			}
			return current.Status.Suspended && current.Status.VPN.ClientSubnet == "10.8.0.0/24" && vpn.Spec.Replicas != nil && *vpn.Spec.Replicas == 1 && gateway.Spec.Replicas != nil && *gateway.Spec.Replicas == 1
		}, 15*time.Second, 250*time.Millisecond).Should(BeTrue())
		var vpn appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: name}, &vpn)).To(Succeed())
		vpn.Status.Replicas = 1
		vpn.Status.ReadyReplicas = 1
		Expect(k8sClient.Status().Update(ctx, &vpn)).To(Succeed())
		Eventually(func() bool {
			var current laboratoryv1alpha1.LabGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil {
				return false
			}
			return current.Status.Suspended && current.Status.VPN.Registered
		}, 15*time.Second, 250*time.Millisecond).Should(BeTrue())
	})

	It("keeps the internet gateway running when only VPN is disabled", func() {
		const name = "vpn-only-disabled"
		group := &laboratoryv1alpha1.LabGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}},
		}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, group) })
		Eventually(func() bool {
			var current laboratoryv1alpha1.LabGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil || current.Status.Namespace == "" {
				return false
			}
			var vpn, gateway appsv1.Deployment
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: name}, &vpn); err != nil {
				return false
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: name}, &gateway); err != nil {
				return false
			}
			return vpn.Spec.Replicas != nil && *vpn.Spec.Replicas == 0 && gateway.Spec.Replicas != nil && *gateway.Spec.Replicas == 1 && !current.Status.VPN.Registered && !current.Status.Suspended && current.Status.VPN.ClientSubnet == "10.8.0.0/24"
		}, 15*time.Second, 250*time.Millisecond).Should(BeTrue())
	})

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

	It("reports suspended while keeping group services running", func() {
		const name = "suspend-status"
		group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, group) })

		Eventually(func() laboratoryv1alpha1.Phase {
			var current laboratoryv1alpha1.LabGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil {
				return ""
			}
			return current.Status.Phase
		}, 15*time.Second, 250*time.Millisecond).Should(Equal(laboratoryv1alpha1.PhaseReady))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, group)).To(Succeed())
		group.Spec.Suspended = true
		Expect(k8sClient.Update(ctx, group)).To(Succeed())

		Eventually(func() bool {
			var current laboratoryv1alpha1.LabGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil {
				return false
			}
			if current.Status.Phase != laboratoryv1alpha1.PhaseSuspended || !current.Status.Suspended {
				return false
			}
			for _, deploymentName := range []string{"vpn", "gateway"} {
				var deployment appsv1.Deployment
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: name}, &deployment); err != nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
					return false
				}
			}
			return true
		}, 15*time.Second, 250*time.Millisecond).Should(BeTrue())
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
