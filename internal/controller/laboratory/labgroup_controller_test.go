package laboratory

import (
	"time"
	
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var _ = Describe(
	"LabGroup controller", func() {
		const (
			timeout = 15 * time.Second
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
