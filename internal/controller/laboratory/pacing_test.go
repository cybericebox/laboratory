package laboratory

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

const (
	testMaxInFlight = 3
	testWaveTimeout = 4 * time.Second
	launchNodeName  = "launch-node"
)

// createLaunchNode registers a node the resource check can count; envtest has no kubelet.
func createLaunchNode(ctx context.Context, c client.Client, cpu string) error {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: launchNodeName}}
	if err := c.Create(ctx, n); err != nil {
		return err
	}
	// The API server taints a new node not-ready; a kubelet would clear it.
	n.Spec.Taints = nil
	if err := c.Update(ctx, n); err != nil {
		return err
	}
	return setLaunchNodeCPU(ctx, c, cpu)
}

func setLaunchNodeCPU(ctx context.Context, c client.Client, cpu string) error {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: launchNodeName}, &n); err != nil {
		return err
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	n.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse("1000Gi"),
	}
	return c.Status().Update(ctx, &n)
}

var _ = Describe("Launch pacing", Ordered, func() {
	const (
		ns       = "default"
		timeout  = 15 * time.Second
		interval = 100 * time.Millisecond
	)

	newLab := func(name, class string) *laboratoryv1alpha1.Lab {
		return &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: laboratoryv1alpha1.LabSpec{
				LaunchClass: class,
				Devices: []laboratoryv1alpha1.DeviceTemplate{
					{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:alpine"},
				},
			},
		}
	}
	get := func(name string) laboratoryv1alpha1.Lab {
		var l laboratoryv1alpha1.Lab
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &l)).To(Succeed())
		return l
	}
	phaseOf := func(name string) laboratoryv1alpha1.Phase {
		var l laboratoryv1alpha1.Lab
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &l); err != nil {
			return ""
		}
		return l.Status.Phase
	}
	admitted := func(names ...string) int {
		n := 0
		for _, name := range names {
			var l laboratoryv1alpha1.Lab
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &l); err == nil && labAdmitted(&l) {
				n++
			}
		}
		return n
	}
	// forceDelete removes labs and their devices, clearing the finalizers no
	// controller runs in this suite.
	forceDelete := func(labNames ...string) {
		for _, name := range labNames {
			var devs laboratoryv1alpha1.DeviceList
			_ = k8sClient.List(ctx, &devs, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: name})
			for i := range devs.Items {
				devs.Items[i].Finalizers = nil
				_ = k8sClient.Update(ctx, &devs.Items[i])
			}
			var l laboratoryv1alpha1.Lab
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &l); err == nil {
				l.Finalizers = nil
				_ = k8sClient.Update(ctx, &l)
				_ = k8sClient.Delete(ctx, &l)
			}
		}
	}
	// markDevicesReady plays the device controller: every device of the lab comes up.
	markDevicesReady := func(lab string) {
		EventuallyWithOffset(1, func(g Gomega) {
			var devs laboratoryv1alpha1.DeviceList
			g.Expect(k8sClient.List(ctx, &devs, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: lab})).To(Succeed())
			g.Expect(devs.Items).NotTo(BeEmpty())
			for i := range devs.Items {
				devs.Items[i].Status.Ready = true
				g.Expect(k8sClient.Status().Update(ctx, &devs.Items[i])).To(Succeed())
			}
		}, timeout, interval).Should(Succeed())
	}

	It("queues labs beyond the in-flight limit and admits the next when one is Ready", func() {
		labs := []string{}
		for i := 1; i <= 5; i++ {
			name := fmt.Sprintf("pace-%d", i)
			labs = append(labs, name)
			Expect(k8sClient.Create(ctx, newLab(name, "pace"))).To(Succeed())
		}
		DeferCleanup(func() { forceDelete(labs...) })

		// The first three are admitted and get devices; the rest wait, with their place.
		Eventually(func() int { return admitted(labs...) }, timeout, interval).Should(Equal(testMaxInFlight))
		for _, name := range labs[:3] {
			Expect(admitted(name)).To(Equal(1), name)
		}
		Eventually(func(g Gomega) {
			for i, name := range labs[3:] {
				l := get(name)
				g.Expect(l.Status.Phase).To(Equal(laboratoryv1alpha1.PhaseQueued))
				g.Expect(l.Status.Launch).NotTo(BeNil())
				g.Expect(l.Status.Launch.Position).To(Equal(int32(i + 1)))
				g.Expect(l.Status.Launch.Length).To(Equal(int32(2)))
				g.Expect(l.Status.Launch.Reason).To(Equal(laboratoryv1alpha1.LaunchReasonInFlightLimit))
				g.Expect(l.Status.Launch.Class).To(Equal("pace"))
			}
		}, timeout, interval).Should(Succeed())

		// Queued labs create nothing; admitted ones do.
		var devs laboratoryv1alpha1.DeviceList
		Expect(k8sClient.List(ctx, &devs, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: labs[3]})).To(Succeed())
		Expect(devs.Items).To(BeEmpty())
		Eventually(func() int {
			var d laboratoryv1alpha1.DeviceList
			_ = k8sClient.List(ctx, &d, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: labs[0]})
			return len(d.Items)
		}, timeout, interval).Should(Equal(1))

		// The first lab becomes Ready: its slot goes to the next in line (pace-4).
		markDevicesReady(labs[0])
		Eventually(func() laboratoryv1alpha1.Phase { return phaseOf(labs[0]) }, timeout, interval).Should(Equal(laboratoryv1alpha1.PhaseReady))
		Eventually(func() int { return admitted(labs[3]) }, timeout, interval).Should(Equal(1))
		l4 := get(labs[3])
		Expect(l4.Status.Launch.AdmittedAt).NotTo(BeNil())
		Expect(l4.Status.Launch.Position).To(BeZero())

		// Slots of labs that never become Ready are released by the wave timeout, so
		// the queue drains: the last lab is admitted too.
		Eventually(func() int { return admitted(labs...) }, timeout, interval).Should(Equal(5))
	})

	It("waits for room and admits when the cluster has it", func() {
		// 300m: one lab of 250m fits, a second does not (headroom 0).
		Expect(setLaunchNodeCPU(ctx, k8sClient, "300m")).To(Succeed())
		DeferCleanup(func() { Expect(setLaunchNodeCPU(ctx, k8sClient, "300")).To(Succeed()) })
		labs := []string{"room-a", "room-b"}
		// Same creation second: the name breaks the tie, room-a goes first.
		Expect(k8sClient.Create(ctx, newLab("room-a", "room"))).To(Succeed())
		Expect(k8sClient.Create(ctx, newLab("room-b", "room"))).To(Succeed())
		DeferCleanup(func() { forceDelete(labs...) })

		Eventually(func() int { return admitted("room-a") }, timeout, interval).Should(Equal(1))
		Eventually(func(g Gomega) {
			l := get("room-b")
			g.Expect(l.Status.Phase).To(Equal(laboratoryv1alpha1.PhaseQueued))
			g.Expect(l.Status.Launch).NotTo(BeNil())
			g.Expect(l.Status.Launch.Reason).To(Equal(laboratoryv1alpha1.LaunchReasonInsufficientResources))
		}, timeout, interval).Should(Succeed())
		Consistently(func() int { return admitted("room-b") }, time.Second, interval).Should(BeZero())

		Expect(setLaunchNodeCPU(ctx, k8sClient, "1")).To(Succeed())
		Eventually(func() int { return admitted("room-b") }, timeout, interval).Should(Equal(1))
	})
})
