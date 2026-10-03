package laboratory

import (
	"time"

	"github.com/cybericebox/laboratory/internal/devices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

var _ = Describe(
	"Lab controller", func() {
		const (
			ns       = "default"
			timeout  = 15 * time.Second
			interval = 250 * time.Millisecond
		)

		It(
			"materializes Device and Connection CRDs and allocates VNI on CREATE", func() {
				lab := &laboratoryv1alpha1.Lab{
					ObjectMeta: metav1.ObjectMeta{Name: "lab-sqli", Namespace: ns},
					Spec: laboratoryv1alpha1.LabSpec{
						Devices: []laboratoryv1alpha1.DeviceTemplate{
							{
								Name: "router1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:latest",
								Interfaces: []laboratoryv1alpha1.InterfaceSpec{
									{
										Name: "eth0",
										Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCP},
									},
								},
							},
							{
								Name: "target1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:latest",
								Interfaces: []laboratoryv1alpha1.InterfaceSpec{
									{
										Name: "eth0",
										Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCP},
									},
								},
							},
						},
						Connections: []laboratoryv1alpha1.ConnectionTemplate{
							{
								Endpoints: []laboratoryv1alpha1.EndpointSpec{
									{Device: "router1", Interface: "eth0"},
									{Device: "target1", Interface: "eth0"},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, lab)).To(Succeed())
				DeferCleanup(
					func() {
						// Clear OVS finalizers from Devices and Connections so they can be GC'd.
						var devList laboratoryv1alpha1.DeviceList
						_ = k8sClient.List(
							ctx, &devList, client.InNamespace(ns),
							client.MatchingLabels{names.LabelLab: "lab-sqli"},
						)
						for i := range devList.Items {
							devList.Items[i].Finalizers = nil
							_ = k8sClient.Update(ctx, &devList.Items[i])
						}
						var connList laboratoryv1alpha1.ConnectionList
						_ = k8sClient.List(
							ctx, &connList, client.InNamespace(ns),
							client.MatchingLabels{names.LabelLab: "lab-sqli"},
						)
						for i := range connList.Items {
							connList.Items[i].Finalizers = nil
							_ = k8sClient.Update(ctx, &connList.Items[i])
						}
						var l laboratoryv1alpha1.Lab
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "lab-sqli", Namespace: ns}, &l)
						l.Finalizers = nil
						_ = k8sClient.Update(ctx, &l)
						_ = k8sClient.Delete(ctx, &l)
					},
				)

				// Two Device CRDs must be created.
				Eventually(
					func() int {
						var list laboratoryv1alpha1.DeviceList
						_ = k8sClient.List(
							ctx, &list, client.InNamespace(ns),
							client.MatchingLabels{names.LabelLab: "lab-sqli"},
						)
						return len(list.Items)
					}, timeout, interval,
				).Should(Equal(2))

				// One Connection CRD must be created.
				Eventually(
					func() int {
						var list laboratoryv1alpha1.ConnectionList
						_ = k8sClient.List(
							ctx, &list, client.InNamespace(ns),
							client.MatchingLabels{names.LabelLab: "lab-sqli"},
						)
						return len(list.Items)
					}, timeout, interval,
				).Should(Equal(1))

				// The Connection must have a VNI allocated in its status.
				Eventually(
					func() bool {
						var list laboratoryv1alpha1.ConnectionList
						_ = k8sClient.List(
							ctx, &list, client.InNamespace(ns),
							client.MatchingLabels{names.LabelLab: "lab-sqli"},
						)
						if len(list.Items) == 0 {
							return false
						}
						return list.Items[0].Status.VNI != nil
					}, timeout, interval,
				).Should(BeTrue())
			},
		)

		It(
			"refuses the device names the platform reserves", func() {
				for _, n := range []string{"vpn", "gateway", "internet"} {
					lab := &laboratoryv1alpha1.Lab{
						ObjectMeta: metav1.ObjectMeta{Name: "reserved-" + n, Namespace: ns},
						Spec: laboratoryv1alpha1.LabSpec{
							Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: n, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx"}},
						},
					}
					err := k8sClient.Create(ctx, lab)
					Expect(err).To(HaveOccurred(), n)
					Expect(err.Error()).To(ContainSubstring("reserved"), n)
				}
			},
		)

		It(
			"allocates VNI for UnmanagedSwitch device", func() {
				lab := &laboratoryv1alpha1.Lab{
					ObjectMeta: metav1.ObjectMeta{Name: "vni-test", Namespace: ns},
					Spec: laboratoryv1alpha1.LabSpec{
						Devices: []laboratoryv1alpha1.DeviceTemplate{
							{Name: "sw1", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
						},
					},
				}
				Expect(k8sClient.Create(ctx, lab)).To(Succeed())
				DeferCleanup(
					func() {
						var devList laboratoryv1alpha1.DeviceList
						_ = k8sClient.List(
							ctx, &devList, client.InNamespace(ns),
							client.MatchingLabels{names.LabelLab: "vni-test"},
						)
						for i := range devList.Items {
							devList.Items[i].Finalizers = nil
							_ = k8sClient.Update(ctx, &devList.Items[i])
						}
						var l laboratoryv1alpha1.Lab
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "vni-test", Namespace: ns}, &l)
						l.Finalizers = nil
						_ = k8sClient.Update(ctx, &l)
						_ = k8sClient.Delete(ctx, &l)
					},
				)

				deviceName := devices.Name("vni-test", "sw1")
				Eventually(
					func() *uint {
						var d laboratoryv1alpha1.Device
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: deviceName, Namespace: ns}, &d)
						return d.Status.VNI
					}, timeout, interval,
				).ShouldNot(BeNil())
			},
		)

		It(
			"sets status=Failed when the switch topology contains a cycle", func() {
				lab := &laboratoryv1alpha1.Lab{
					ObjectMeta: metav1.ObjectMeta{Name: "lab-cycle", Namespace: ns},
					Spec: laboratoryv1alpha1.LabSpec{
						Devices: []laboratoryv1alpha1.DeviceTemplate{
							{Name: "sw1", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
							{Name: "sw2", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
							{Name: "sw3", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
						},
						Connections: []laboratoryv1alpha1.ConnectionTemplate{
							{Endpoints: []laboratoryv1alpha1.EndpointSpec{{Device: "sw1"}, {Device: "sw2"}}},
							{Endpoints: []laboratoryv1alpha1.EndpointSpec{{Device: "sw2"}, {Device: "sw3"}}},
							{Endpoints: []laboratoryv1alpha1.EndpointSpec{{Device: "sw3"}, {Device: "sw1"}}},
						},
					},
				}
				Expect(k8sClient.Create(ctx, lab)).To(Succeed())
				DeferCleanup(
					func() {
						var l laboratoryv1alpha1.Lab
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "lab-cycle", Namespace: ns}, &l)
						l.Finalizers = nil
						_ = k8sClient.Update(ctx, &l)
						_ = k8sClient.Delete(ctx, &l)
					},
				)

				Eventually(
					func() laboratoryv1alpha1.Phase {
						var updated laboratoryv1alpha1.Lab
						_ = k8sClient.Get(ctx, types.NamespacedName{Name: "lab-cycle", Namespace: ns}, &updated)
						return updated.Status.Phase
					}, timeout, interval,
				).Should(Equal(laboratoryv1alpha1.PhaseFailed))

				// No Device or Connection CRDs must be created for a cyclic topology.
				var devList laboratoryv1alpha1.DeviceList
				Expect(
					k8sClient.List(
						ctx, &devList, client.InNamespace(ns),
						client.MatchingLabels{names.LabelLab: "lab-cycle"},
					),
				).To(Succeed())
				Expect(devList.Items).To(BeEmpty())
			},
		)
	},
)
