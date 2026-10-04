package laboratory

import (
	"fmt"
	"time"

	"github.com/cybericebox/laboratory/internal/devices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

var workloadSpecNo int

var _ = Describe("Workload names and user labels", func() {
	const (
		timeout  = 15 * time.Second
		interval = 100 * time.Millisecond
	)
	var ns string
	var dr *DeviceReconciler

	BeforeEach(func() {
		workloadSpecNo++
		ns = fmt.Sprintf("wname-%d", workloadSpecNo)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		dr = &DeviceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Defaults: DeviceDefaults{CPU: "100m", Memory: "100Mi"}}
	})
	reconcileDevice := func(name string) {
		_, err := dr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	newDevice := func(lab, code string, labels map[string]string, state *laboratoryv1alpha1.DeviceStateSpec) string {
		name := lab + "-web"
		ExpectWithOffset(1, k8sClient.Create(ctx, &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: laboratoryv1alpha1.DeviceSpec{
				Type: laboratoryv1alpha1.DeviceTypeContainer, Name: "web", Code: code, LabRef: lab, Image: "nginx:alpine", State: state,
			},
		})).To(Succeed())
		return name
	}

	It("names the Deployments of two labs' devices <device>-<code>, never after the lab", func() {
		a := newDevice("alpha", "aaa", map[string]string{names.LabelLab: "alpha", "team": "red"}, nil)
		b := newDevice("beta", "bbb", map[string]string{names.LabelLab: "beta"}, nil)
		reconcileDevice(a)
		reconcileDevice(b)
		var list appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
		got := map[string]*appsv1.Deployment{}
		for i := range list.Items {
			got[list.Items[i].Name] = &list.Items[i]
		}
		Expect(got).To(HaveLen(2))
		Expect(got).To(HaveKey("web-aaa"))
		Expect(got).To(HaveKey("web-bbb"))
		// The relation to the lab is the owner reference and the labels.
		Expect(got["web-aaa"].OwnerReferences).To(HaveLen(1))
		Expect(got["web-aaa"].OwnerReferences[0].Name).To(Equal("alpha-web"))
		Expect(got["web-aaa"].Spec.Template.Labels).To(HaveKeyWithValue(names.LabelLab, "alpha"))
		Expect(got["web-bbb"].Spec.Template.Labels).To(HaveKeyWithValue(names.LabelLab, "beta"))
		// The user labels of the lab are on the pod template.
		Expect(got["web-aaa"].Spec.Template.Labels).To(HaveKeyWithValue("team", "red"))
		Expect(got["web-bbb"].Spec.Template.Labels).NotTo(HaveKey("team"))
	})

	It("names the bare pods of snapshot-backed devices <device>-<code>-<incarnation>", func() {
		state := &laboratoryv1alpha1.DeviceStateSpec{Enabled: true, MaxLayers: 10}
		a := newDevice("alpha", "aaa", nil, state)
		b := newDevice("beta", "bbb", nil, state)
		for i := 0; i < 2; i++ {
			reconcileDevice(a)
			reconcileDevice(b)
		}
		for _, name := range []string{"web-aaa-1", "web-bbb-1"} {
			var p corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)).To(Succeed(), name)
		}
	})

	It("keeps a device without a code under its legacy name", func() {
		name := newDevice("old", "", nil, nil)
		reconcileDevice(name)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep)).To(Succeed())
	})

	It("keeps the user labels of the live pods equal to those of the device", func() {
		name := newDevice("alpha", "aaa", map[string]string{names.LabelLab: "alpha", "team": "red", "env": "prod"}, nil)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "web-aaa-xyz", Namespace: ns,
				Labels: map[string]string{names.LabelLab: "alpha", names.LabelDevice: "web", "app": "web"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		reconcileDevice(name)
		get := func() map[string]string {
			var p corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod.Name}, &p)).To(Succeed())
			return p.Labels
		}
		Expect(get()).To(And(HaveKeyWithValue("team", "red"), HaveKeyWithValue("env", "prod"), HaveKeyWithValue("app", "web")))

		var d laboratoryv1alpha1.Device
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d)).To(Succeed())
		d.Labels = map[string]string{names.LabelLab: "alpha", "team": "blue"}
		Expect(k8sClient.Update(ctx, &d)).To(Succeed())
		reconcileDevice(name)
		labels := get()
		Expect(labels).To(And(HaveKeyWithValue("team", "blue"), HaveKeyWithValue("app", "web"), HaveKeyWithValue(names.LabelLab, "alpha")))
		Expect(labels).NotTo(HaveKey("env"))
	})

	It("copies the user labels of a Lab onto its devices, never the reserved ones, and follows updates", func() {
		lab := &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{
				Name: "labeled", Namespace: ns,
				Labels: map[string]string{"team": "red", "env": "prod", names.LabelDeployGroup: "g1"},
			},
			Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
				{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:alpine"},
			}},
		}
		Expect(k8sClient.Create(ctx, lab)).To(Succeed())
		DeferCleanup(func() {
			var devs laboratoryv1alpha1.DeviceList
			_ = k8sClient.List(ctx, &devs, client.InNamespace(ns))
			for i := range devs.Items {
				devs.Items[i].Finalizers = nil
				_ = k8sClient.Update(ctx, &devs.Items[i])
			}
			var l laboratoryv1alpha1.Lab
			if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "labeled"}, &l) == nil {
				l.Finalizers = nil
				_ = k8sClient.Update(ctx, &l)
				_ = k8sClient.Delete(ctx, &l)
			}
		})
		device := func() laboratoryv1alpha1.Device {
			var d laboratoryv1alpha1.Device
			_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: devices.Name("labeled", "web")}, &d)
			return d
		}
		Eventually(func() map[string]string { return device().Labels }, timeout, interval).Should(And(
			HaveKeyWithValue("team", "red"), HaveKeyWithValue("env", "prod"), HaveKeyWithValue(names.LabelLab, "labeled")))
		Expect(device().Labels).NotTo(HaveKey(names.LabelDeployGroup))
		Expect(device().Spec.Code).To(MatchRegexp(`^[a-z0-9]{3}$`))

		var cur laboratoryv1alpha1.Lab
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "labeled"}, &cur)).To(Succeed())
		cur.Labels = map[string]string{"team": "blue", names.LabelDeployGroup: "g2"}
		Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
		Eventually(func() map[string]string { return device().Labels }, timeout, interval).Should(And(
			HaveKeyWithValue("team", "blue"), Not(HaveKey("env")), HaveKeyWithValue(names.LabelLab, "labeled")))
	})
})
