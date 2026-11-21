package laboratory

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func vpnTestPod(name, ip string, ready bool, age time.Duration) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lg-g", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}, CreationTimestamp: metav1.NewTime(time.Unix(1_700_000_000, 0).Add(age))},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}}},
	}
}

// The backend is the address of the ready VPN pod and nothing else: empty while the deployment is not ready, for a pod that is starting,
// terminating or has no address, and for pods that are not the group's VPN; with two ready pods the newest.
func TestVPNBackendIsTheReadyPodAddress(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	deleting := vpnTestPod("old", "10.244.9.9", true, 0)
	now := metav1.Now()
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"held"}
	pending := vpnTestPod("pending", "10.244.8.8", true, time.Hour)
	pending.Status.Phase = corev1.PodPending
	device := vpnTestPod("web", "10.244.7.7", true, time.Hour)
	device.Labels = map[string]string{names.LabelLab: "lab", names.LabelDevice: "web"}
	for _, tc := range []struct {
		name  string
		ready bool
		pods  []client.Object
		want  string
	}{
		{"a ready pod", true, []client.Object{vpnTestPod("vpn-a", "10.244.1.5", true, 0)}, "10.244.1.5:51820"},
		{"deployment not ready", false, []client.Object{vpnTestPod("vpn-a", "10.244.1.5", true, 0)}, ""},
		{"no pod yet", true, nil, ""},
		{"pod not ready", true, []client.Object{vpnTestPod("vpn-a", "10.244.1.5", false, 0)}, ""},
		{"no address", true, []client.Object{vpnTestPod("vpn-a", "", true, 0)}, ""},
		{"a name is not an address", true, []client.Object{vpnTestPod("vpn-a", "vpn.lg-g.svc.cluster.local", true, 0)}, ""},
		{"loopback", true, []client.Object{vpnTestPod("vpn-a", "127.0.0.1", true, 0)}, ""},
		{"terminating", true, []client.Object{deleting}, ""},
		{"pending", true, []client.Object{pending}, ""},
		{"a device pod is not the VPN", true, []client.Object{device}, ""},
		{"the newest of two ready pods", true, []client.Object{vpnTestPod("vpn-a", "10.244.1.5", true, 0), vpnTestPod("vpn-b", "10.244.2.6", true, time.Minute)}, "10.244.2.6:51820"},
		{"a terminating old pod is passed over", true, []client.Object{deleting, vpnTestPod("vpn-b", "10.244.2.6", true, time.Minute)}, "10.244.2.6:51820"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &LabGroupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.pods...).Build()}
			got, err := r.vpnBackend(context.Background(), "lg-g", tc.ready)
			if err != nil || got != tc.want {
				t.Fatalf("backend %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}

var _ = Describe("LabGroup VPN backend", func() {
	It("publishes the address of the ready VPN pod, follows a replacement of the pod and drops it when the pod is gone", func() {
		const name = "vpn-backend-follows"
		group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, group) })
		ns := laboratoryv1alpha1.LabGroupNamespace(name)
		Eventually(func() error {
			var vpn appsv1.Deployment
			return k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &vpn)
		}, 15*time.Second, 250*time.Millisecond).Should(Succeed())
		var vpn appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: ns}, &vpn)).To(Succeed())
		vpn.Status.Replicas, vpn.Status.ReadyReplicas = 1, 1
		Expect(k8sClient.Status().Update(ctx, &vpn)).To(Succeed())

		backend := func() string {
			var current laboratoryv1alpha1.LabGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &current); err != nil {
				return "?"
			}
			return current.Status.VPN.Backend
		}
		Eventually(backend, 15*time.Second, 250*time.Millisecond).Should(BeEmpty())

		makePod := func(podName, ip string) *corev1.Pod {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns, Labels: map[string]string{names.LabelComponent: names.ComponentVPN}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vpn", Image: "test"}}},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}},
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
			return p
		}
		first := makePod("vpn-first", "10.244.1.5")
		Eventually(backend, 15*time.Second, 250*time.Millisecond).Should(Equal("10.244.1.5:51820"))

		// the pod is replaced: the address in the status follows (no restart of anything), and never keeps the old one
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		makePod("vpn-second", "10.244.2.6")
		Eventually(backend, 15*time.Second, 250*time.Millisecond).Should(Equal("10.244.2.6:51820"))

		// no ready pod any more: no address
		var second corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpn-second", Namespace: ns}, &second)).To(Succeed())
		second.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		Expect(k8sClient.Status().Update(ctx, &second)).To(Succeed())
		Eventually(backend, 15*time.Second, 250*time.Millisecond).Should(BeEmpty())
	})
})
