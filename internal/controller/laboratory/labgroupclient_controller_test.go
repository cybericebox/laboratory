package laboratory

import (
	"fmt"
	"sync/atomic"
	"time"
	
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	
	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// lgcCounter gives each BeforeEach a unique LabGroup name, avoiding
// "object is being deleted" conflicts when a finalizer-held deletion from a
// previous spec hasn't finished by the time the next BeforeEach runs.
var lgcCounter atomic.Int64

var _ = Describe(
	"LabGroupClient controller", func() {
		const (
			timeout = 15 * time.Second
			interval = 250 * time.Millisecond
		)
		
		// Each test gets its own LabGroup (and thus namespace) to avoid pool state leakage.
		var (
			lgName string
			lgNS   string
		)
		
		BeforeEach(
			func() {
				lg := &laboratoryv1alpha1.LabGroup{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("lgc-test-%d", lgcCounter.Add(1))},
				}
				lgName = lg.Name
				Expect(k8sClient.Create(ctx, lg)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, lg)
					},
				)
				
				// Wait until LabGroup reconciler populates Status.Namespace (and creates the vpn-clients-0 pool).
				Eventually(
					func() string {
						var updated laboratoryv1alpha1.LabGroup
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: lgName}, &updated)
						return updated.Status.Namespace
					}, timeout, interval,
				).ShouldNot(BeEmpty())
				
				var updated laboratoryv1alpha1.LabGroup
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lgName}, &updated)).To(Succeed())
				lgNS = updated.Status.Namespace
			},
		)
		
		It(
			"assigns a unique non-zero IP and creates client Secret on CREATE", func() {
				lgc := &laboratoryv1alpha1.LabGroupClient{
					ObjectMeta: metav1.ObjectMeta{Name: "client-a", Namespace: lgNS},
				}
				Expect(k8sClient.Create(ctx, lgc)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, lgc)
					},
				)
				
				// Wait for AssignedIP to be set.
				Eventually(
					func() string {
						var updated laboratoryv1alpha1.LabGroupClient
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "client-a", Namespace: lgNS}, &updated)
						return updated.Status.AssignedIP
					}, timeout, interval,
				).ShouldNot(BeEmpty())
				
				var updated laboratoryv1alpha1.LabGroupClient
				Expect(
					k8sClient.Get(
						ctx,
						types.NamespacedName{Name: "client-a", Namespace: lgNS},
						&updated,
					),
				).To(Succeed())
				
				// IP must be a valid 10.8.0.X/32 and X must not be 0 (reserved).
				Expect(updated.Status.AssignedIP).To(MatchRegexp(`^10\.8\.0\.[1-9][0-9]*/32$`))
				Expect(updated.Status.SecretRef).NotTo(BeEmpty())
				
				// Secret must exist and contain the required keys.
				var secret corev1.Secret
				Expect(
					k8sClient.Get(
						ctx,
						types.NamespacedName{Name: updated.Status.SecretRef, Namespace: lgNS},
						&secret,
					),
				).To(Succeed())
				Expect(secret.Data).To(HaveKey("publicKey"))
				Expect(secret.Data).To(HaveKey("assignedIP"))
			},
		)
		
		It(
			"assigns distinct IPs to two concurrent clients", func() {
				lgcA := &laboratoryv1alpha1.LabGroupClient{
					ObjectMeta: metav1.ObjectMeta{Name: "client-b", Namespace: lgNS},
				}
				lgcB := &laboratoryv1alpha1.LabGroupClient{
					ObjectMeta: metav1.ObjectMeta{Name: "client-c", Namespace: lgNS},
				}
				Expect(k8sClient.Create(ctx, lgcA)).To(Succeed())
				Expect(k8sClient.Create(ctx, lgcB)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, lgcA)
						_ = k8sClient.Delete(ctx, lgcB)
					},
				)
				
				ipOf := func(name string) string {
					var lgc laboratoryv1alpha1.LabGroupClient
					_ = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: lgNS}, &lgc)
					return lgc.Status.AssignedIP
				}
				
				Eventually(
					func() bool {
						return ipOf("client-b") != "" && ipOf("client-c") != ""
					}, timeout, interval,
				).Should(BeTrue())
				
				Expect(ipOf("client-b")).NotTo(Equal(ipOf("client-c")))
			},
		)
		
		It(
			"releases the IP back to the pool on DELETE", func() {
				lgc := &laboratoryv1alpha1.LabGroupClient{
					ObjectMeta: metav1.ObjectMeta{Name: "client-d", Namespace: lgNS},
				}
				Expect(k8sClient.Create(ctx, lgc)).To(Succeed())
				
				// Wait for IP assignment.
				Eventually(
					func() string {
						var updated laboratoryv1alpha1.LabGroupClient
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "client-d", Namespace: lgNS}, &updated)
						return updated.Status.AssignedIP
					}, timeout, interval,
				).ShouldNot(BeEmpty())
				
				var updated laboratoryv1alpha1.LabGroupClient
				Expect(
					k8sClient.Get(
						ctx,
						types.NamespacedName{Name: "client-d", Namespace: lgNS},
						&updated,
					),
				).To(Succeed())
				assignedIP := updated.Status.AssignedIP
				
				// Record free count before delete.
				freeBefore := poolFreeCount(lgNS)
				
				// Delete the client and wait for it to be gone.
				Expect(k8sClient.Delete(ctx, &updated)).To(Succeed())
				Eventually(
					func() bool {
						var check laboratoryv1alpha1.LabGroupClient
						return k8sClient.Get(
							ctx,
							types.NamespacedName{Name: "client-d", Namespace: lgNS},
							&check,
						) != nil
					}, timeout, interval,
				).Should(BeTrue())
				
				// Free count must increase by 1 (the released slot).
				Expect(poolFreeCount(lgNS)).To(Equal(freeBefore + 1))
				
				// The same IP must be reallocatable.
				lgc2 := &laboratoryv1alpha1.LabGroupClient{
					ObjectMeta: metav1.ObjectMeta{Name: "client-d2", Namespace: lgNS},
				}
				Expect(k8sClient.Create(ctx, lgc2)).To(Succeed())
				DeferCleanup(
					func() {
						_ = k8sClient.Delete(ctx, lgc2)
					},
				)
				
				Eventually(
					func() string {
						var updated2 laboratoryv1alpha1.LabGroupClient
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "client-d2", Namespace: lgNS}, &updated2)
						return updated2.Status.AssignedIP
					}, timeout, interval,
				).Should(Equal(assignedIP))
			},
		)
	},
)

// poolFreeCount returns the total free slots across all vpn-clients pools in ns.
func poolFreeCount(ns string) uint {
	var list allocationv1alpha1.PoolList
	Expect(
		k8sClient.List(
			ctx, &list,
			client.InNamespace(ns),
			client.MatchingLabels{poolpkg.PoolGroupLabel: "vpn-clients"},
		),
	).To(Succeed())
	var total uint
	for _, p := range list.Items {
		total += p.Status.Free
	}
	return total
}
