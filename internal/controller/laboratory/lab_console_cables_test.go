package laboratory

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/consolespec"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

var _ = Describe("Console cables", func() {
	const (
		ns       = "default"
		timeout  = 15 * time.Second
		interval = 250 * time.Millisecond
	)
	svc := func() *laboratoryv1alpha1.DeviceService {
		return &laboratoryv1alpha1.DeviceService{Command: []string{"/srv"}, User: "root"}
	}
	cable := func(sd, sp, cd, cp string) laboratoryv1alpha1.ConsoleCableTemplate {
		return laboratoryv1alpha1.ConsoleCableTemplate{Serial: laboratoryv1alpha1.PortRef{Device: sd, Port: sp}, Console: laboratoryv1alpha1.PortRef{Device: cd, Port: cp}}
	}
	cleanup := func(name string) {
		DeferCleanup(func() {
			var devList laboratoryv1alpha1.DeviceList
			_ = k8sClient.List(ctx, &devList, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: name})
			for i := range devList.Items {
				devList.Items[i].Finalizers = nil
				_ = k8sClient.Update(ctx, &devList.Items[i])
			}
			var l laboratoryv1alpha1.Lab
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &l)
			l.Finalizers = nil
			_ = k8sClient.Update(ctx, &l)
			_ = k8sClient.Delete(ctx, &l)
		})
	}
	cablesOf := func(lab string) []laboratoryv1alpha1.ConsoleCable {
		var list laboratoryv1alpha1.ConsoleCableList
		_ = k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: lab})
		return list.Items
	}

	It("materializes the ports on the Devices and one ConsoleCable per cable, owned by the Lab, never a Connection", func() {
		lab := &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-console", Namespace: ns},
			Spec: laboratoryv1alpha1.LabSpec{
				Devices: []laboratoryv1alpha1.DeviceTemplate{
					{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "frr", Service: svc(),
						ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0", Command: []string{"/usr/bin/vtysh"}}, {Name: "aux0"}}},
					{Name: "pc1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "ubuntu", Service: svc(),
						SerialPorts: []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}, {Name: "serial1"}}},
				},
				ConsoleCables: []laboratoryv1alpha1.ConsoleCableTemplate{cable("pc1", "serial0", "r1", "console0"), cable("pc1", "serial1", "r1", "aux0")},
			},
		}
		Expect(k8sClient.Create(ctx, lab)).To(Succeed())
		cleanup(lab.Name)

		Eventually(func() int { return len(cablesOf(lab.Name)) }, timeout, interval).Should(Equal(2))
		var got laboratoryv1alpha1.Lab
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lab), &got)).To(Succeed())
		for _, c := range cablesOf(lab.Name) {
			Expect(c.Spec.LabRef).To(Equal(lab.Name))
			Expect(c.Spec.Serial.Device).To(Equal("pc1"))
			Expect(c.Spec.Console.Device).To(Equal("r1"))
			Expect(metav1.IsControlledBy(&c, &got) || len(c.OwnerReferences) == 1 && c.OwnerReferences[0].UID == got.UID).To(BeTrue())
		}
		var conns laboratoryv1alpha1.ConnectionList
		Expect(k8sClient.List(ctx, &conns, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: lab.Name})).To(Succeed())
		Expect(conns.Items).To(BeEmpty())

		// the Devices carry their ports
		Eventually(func() bool {
			var devs laboratoryv1alpha1.DeviceList
			_ = k8sClient.List(ctx, &devs, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: lab.Name})
			ok := 0
			for _, d := range devs.Items {
				if d.Spec.Name == "r1" && len(d.Spec.ConsolePorts) == 2 || d.Spec.Name == "pc1" && len(d.Spec.SerialPorts) == 2 {
					ok++
				}
			}
			return ok == 2
		}, timeout, interval).Should(BeTrue())

		// the lab status lists the cables
		Eventually(func() int {
			var l laboratoryv1alpha1.Lab
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(lab), &l)
			return len(l.Status.ConsoleCables)
		}, timeout, interval).Should(Equal(2))

		// unplugging a cable in the spec removes its object
		Eventually(func() error {
			var l laboratoryv1alpha1.Lab
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(lab), &l); err != nil {
				return err
			}
			l.Spec.ConsoleCables = l.Spec.ConsoleCables[:1]
			return k8sClient.Update(ctx, &l)
		}, timeout, interval).Should(Succeed())
		Eventually(func() int { return len(cablesOf(lab.Name)) }, timeout, interval).Should(Equal(1))
	})

	It("fails a lab whose cable does not resolve, with the reason ConsoleCableInvalid", func() {
		lab := &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-console-bad", Namespace: ns},
			Spec: laboratoryv1alpha1.LabSpec{
				Devices: []laboratoryv1alpha1.DeviceTemplate{
					{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "frr", Service: svc(),
						ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}},
					{Name: "pc1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "ubuntu", Service: svc(),
						SerialPorts: []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}},
				},
				ConsoleCables: []laboratoryv1alpha1.ConsoleCableTemplate{cable("pc1", "serial0", "r1", "aux0")},
			},
		}
		Expect(k8sClient.Create(ctx, lab)).To(Succeed())
		cleanup(lab.Name)
		Eventually(func() string {
			var l laboratoryv1alpha1.Lab
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(lab), &l)
			if c := meta.FindStatusCondition(l.Status.Conditions, labstatus.ConditionReady); c != nil {
				return c.Reason
			}
			return ""
		}, timeout, interval).Should(Equal(consolespec.ReasonInvalid))
		Expect(cablesOf(lab.Name)).To(BeEmpty())
	})
})
