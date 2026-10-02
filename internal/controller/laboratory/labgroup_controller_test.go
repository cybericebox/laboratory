package laboratory

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/names"
)

var _ = Describe("LabGroup suspension", func() {
	It("passes the configured support address to the VPN server", func() {
		const namespace = "default"
		r := &LabGroupReconciler{Client: k8sClient, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "test", SupportEmail: "help@example.org"}
		Expect(r.ensureVPNDeployment(ctx, namespace, false)).To(Succeed())
		DeferCleanup(func() {
			var dep appsv1.Deployment
			if k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: namespace}, &dep) == nil {
				_ = k8sClient.Delete(ctx, &dep)
			}
		})
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: namespace}, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "SUPPORT_EMAIL", Value: "help@example.org"}))
		// No privileged init container any more: the node-agent switches conntrack byte accounting on when it wires
		// the pod (the annotation asks for it), and the VPN container keeps only NET_ADMIN and NET_RAW.
		Expect(dep.Spec.Template.Spec.InitContainers).To(BeEmpty())
		Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue("network.cybericebox.com/conntrack-accounting", "true"))
		sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
		Expect(sc.Privileged).To(BeNil())
		Expect(sc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
		Expect(sc.Capabilities.Add).To(ConsistOf(corev1.Capability("NET_ADMIN"), corev1.Capability("NET_RAW")))
		r.SupportEmail = "new-help@example.org"
		Expect(r.ensureVPNDeployment(ctx, namespace, false)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: namespace}, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "SUPPORT_EMAIL", Value: "new-help@example.org"}))
	})

	It("gives the VPN and gateway pods Guaranteed resources, and a changed value reaches an existing group", func() {
		const namespace = "default"
		r := &LabGroupReconciler{Client: k8sClient, VPNBaseNetwork: "10.8.0.0/10", InetBaseNetwork: "10.9.0.0/10", VPNImage: "test", GatewayImage: "test",
			GroupPods: grouppods.Config{VPNCPU: "120m", VPNMemory: "80Mi", GatewayCPU: "60m", GatewayMemory: "40Mi"}}
		Expect(r.ensureVPNDeployment(ctx, namespace, false)).To(Succeed())
		Expect(r.ensureGatewayDeployment(ctx, namespace, false)).To(Succeed())
		DeferCleanup(func() {
			for _, n := range []string{"vpn", "gateway"} {
				var dep appsv1.Deployment
				if k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, &dep) == nil {
					_ = k8sClient.Delete(ctx, &dep)
				}
			}
		})
		resOf := func(name string) corev1.ResourceRequirements {
			var dep appsv1.Deployment
			ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)).To(Succeed())
			return dep.Spec.Template.Spec.Containers[0].Resources
		}
		vpn, gw := resOf("vpn"), resOf("gateway")
		Expect(quantity(vpn.Requests, corev1.ResourceCPU)).To(Equal("120m"))
		Expect(quantity(vpn.Limits, corev1.ResourceMemory)).To(Equal("80Mi"))
		Expect(vpn.Requests).To(Equal(vpn.Limits))
		Expect(quantity(gw.Requests, corev1.ResourceCPU)).To(Equal("60m"))
		Expect(gw.Requests).To(Equal(gw.Limits))
		// A later change of the chart value reaches the group that exists (a rolling update of its pods, see rollout.go).
		r.GroupPods = grouppods.Config{VPNCPU: "500m", VPNMemory: "512Mi", GatewayCPU: "500m", GatewayMemory: "512Mi"}
		Expect(r.ensureVPNDeployment(ctx, namespace, false)).To(Succeed())
		Expect(r.ensureGatewayDeployment(ctx, namespace, false)).To(Succeed())
		Expect(quantity(resOf("vpn").Requests, corev1.ResourceCPU)).To(Equal("500m"))
		Expect(quantity(resOf("gateway").Requests, corev1.ResourceMemory)).To(Equal("512Mi"))
	})

	It("binds the operator to its working role in a namespace, and repairs the binding", func() {
		const namespace = "default"
		r := &LabGroupReconciler{Client: k8sClient, OperatorSA: types.NamespacedName{Namespace: "laboratory-system", Name: "laboratory-controller-manager"}}
		Expect(r.ensureOperatorBinding(ctx, namespace)).To(Succeed())
		DeferCleanup(func() {
			var rb rbacv1.RoleBinding
			if k8sClient.Get(ctx, types.NamespacedName{Name: names.OperatorRoleBindingName, Namespace: namespace}, &rb) == nil {
				_ = k8sClient.Delete(ctx, &rb)
			}
		})
		var rb rbacv1.RoleBinding
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: names.OperatorRoleBindingName, Namespace: namespace}, &rb)).To(Succeed())
		Expect(rb.RoleRef.Kind).To(Equal("ClusterRole"))
		Expect(rb.RoleRef.Name).To(Equal("laboratory-operator-namespaced"))
		Expect(rb.Subjects).To(ConsistOf(rbacv1.Subject{Kind: "ServiceAccount", Name: "laboratory-controller-manager", Namespace: "laboratory-system"}))
		// A binding someone changed is put back; without an operator identity nothing is made.
		rb.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "intruder", Namespace: "x"}}
		Expect(k8sClient.Update(ctx, &rb)).To(Succeed())
		Expect(r.ensureOperatorBinding(ctx, namespace)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: names.OperatorRoleBindingName, Namespace: namespace}, &rb)).To(Succeed())
		Expect(rb.Subjects[0].Name).To(Equal("laboratory-controller-manager"))
		Expect((&LabGroupReconciler{Client: k8sClient}).ensureOperatorBinding(ctx, "kube-system")).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: names.OperatorRoleBindingName, Namespace: "kube-system"}, &rb)).NotTo(Succeed())
	})

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
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &vpnService); err != nil || len(vpnService.Spec.Ports) != 1 || vpnService.Spec.Ports[0].Protocol != corev1.ProtocolUDP || vpnService.Spec.Ports[0].Port != 51820 {
				return false
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &vpn); err != nil {
				return false
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &gateway); err != nil {
				return false
			}
			return current.Status.Suspended && current.Status.VPN.ClientSubnet == "10.8.0.0/24" && vpn.Spec.Replicas != nil && *vpn.Spec.Replicas == 1 && gateway.Spec.Replicas != nil && *gateway.Spec.Replicas == 1
		}, 15*time.Second, 250*time.Millisecond).Should(BeTrue())
		var vpn appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &vpn)).To(Succeed())
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
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &vpn); err != nil {
				return false
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &gateway); err != nil {
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

		r := &LabGroupReconciler{Client: k8sClient, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "test"}
		Expect(r.ensureVPNDeployment(ctx, namespace, true)).To(Succeed())
		Expect(r.ensureGatewayDeployment(ctx, namespace, true)).To(Succeed())

		for _, name := range []string{"vpn", "gateway"} {
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(0)))
		}

		// A deployment made before the hardening is brought to it: the node-agent annotation, no privileged init
		// container, dropped capabilities, the runtime's seccomp profile.
		for name, caps := range map[string][]corev1.Capability{"vpn": {"NET_ADMIN", "NET_RAW"}, "gateway": {"NET_ADMIN", "NET_BIND_SERVICE", "NET_RAW"}} {
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(BeEmpty())
			sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
			Expect(sc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(sc.Capabilities.Add).To(Equal(caps))
			Expect(dep.Spec.Template.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		}
		var vpn appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: namespace}, &vpn)).To(Succeed())
		Expect(vpn.Spec.Template.Annotations).To(HaveKeyWithValue("network.cybericebox.com/conntrack-accounting", "true"))
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
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: laboratoryv1alpha1.LabGroupNamespace(name)}, &deployment); err != nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
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

		It(
			"guards every pod of the group with one PodDisruptionBudget", func() {
				lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "pdb-labgroup"}}
				Expect(k8sClient.Create(ctx, lg)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, lg) })

				Eventually(
					func(g Gomega) {
						var pdb policyv1.PodDisruptionBudget
						g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: groupDisruptionBudgetName, Namespace: laboratoryv1alpha1.LabGroupNamespace("pdb-labgroup")}, &pdb)).To(Succeed())
						g.Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
						g.Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(0))
						g.Expect(pdb.Spec.Selector).NotTo(BeNil())
						g.Expect(pdb.Spec.Selector.MatchLabels).To(BeEmpty())
						g.Expect(pdb.Spec.Selector.MatchExpressions).To(BeEmpty())
					}, timeout, interval,
				).Should(Succeed())
			},
		)
	},
)

// quantity renders one resource of a list.
func quantity(l corev1.ResourceList, name corev1.ResourceName) string {
	q := l[name]
	return q.String()
}
