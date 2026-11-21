package laboratory

import (
	"context"
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

var _ = Describe("Device bootstrap: a device with state persistence", func() {
	ctx := context.Background()

	var (
		clock  time.Time
		reg    *fakeRegistry
		r      *DeviceReconciler
		ns     string
		dev    *laboratoryv1alpha1.Device
		req    reconcile.Request
		testNo int
	)

	const base = "nginx:alpine"
	const snap = "localhost:5035/lab/x/lab/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	reconcileOnce := func() reconcile.Result {
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	getDevice := func() *laboratoryv1alpha1.Device {
		var d laboratoryv1alpha1.Device
		Expect(k8sClient.Get(ctx, req.NamespacedName, &d)).To(Succeed())
		return &d
	}
	getPod := func(n int32) (*corev1.Pod, error) {
		var p corev1.Pod
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("%s-%d", dev.Name, n)}, &p)
		return &p, err
	}
	mustPod := func(n int32) *corev1.Pod {
		p, err := getPod(n)
		Expect(err).NotTo(HaveOccurred())
		return p
	}
	patchState := func(mutate func(*laboratoryv1alpha1.DeviceStateStatus)) {
		d := getDevice()
		orig := d.DeepCopy()
		mutate(d.Status.State)
		Expect(k8sClient.Status().Patch(ctx, d, client.MergeFrom(orig))).To(Succeed())
	}
	// running: the launcher finished the bootstrap (the startup probe passed) and the service runs.
	setRunning := func(p *corev1.Pod, started bool) {
		p.Status.Phase = corev1.PodRunning
		p.Status.PodIP = "10.1.2.3"
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "web", Started: &started, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	// ended: the container ended with the message the launcher leaves.
	setEnded := func(p *corev1.Pod, message string, code int32) {
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "web", Started: ptrBool(false), State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Message: message, StartedAt: metav1.NewTime(clock.Add(-time.Minute)), FinishedAt: metav1.NewTime(clock),
		}}}}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	exitSnapshotDone := func(p *corev1.Pod, image string) {
		patchState(func(s *laboratoryv1alpha1.DeviceStateStatus) {
			s.ExitSnapshotPod = p.Name
			if image != "" {
				s.Image = image
			}
		})
	}
	failedMessage := func(attempt int, final bool) string {
		return fmt.Sprintf(`{"kind":"failed","attempt":%d,"final":%t,"reason":"the script exited with code 1"}`, attempt, final)
	}
	// nextPod ends the pod with a failed bootstrap and returns the pod that replaces it.
	failAttempt := func(n int32, attempt int, final bool) {
		p := mustPod(n)
		setEnded(p, failedMessage(attempt, final), 78)
		reconcileOnce() // the pod ended: record, wait for the exit snapshot
		exitSnapshotDone(p, snap)
		reconcileOnce() // delete the ended pod
		clock = clock.Add(5 * time.Minute)
		reconcileOnce() // observe that it is gone
	}

	BeforeEach(func() {
		testNo++
		ns = fmt.Sprintf("boot-%d", testNo)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		clock = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
		reg = &fakeRegistry{}
		r = &DeviceReconciler{
			Reader: standaloneRunningLabReader{k8sClient}, Client: k8sClient,
			Scheme:              k8sClient.Scheme(),
			Registry:            reg,
			Now:                 func() time.Time { return clock },
			ExitSnapshotTimeout: 30 * time.Second,
			NetConfigImage:      "registry/node:1",
		}
		dev = &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-web", Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{
				Type: laboratoryv1alpha1.DeviceTypeContainer, Name: "web", LabRef: "lab", Image: base,
				State:     &laboratoryv1alpha1.DeviceStateSpec{Enabled: true, MaxLayers: 10},
				Bootstrap: &laboratoryv1alpha1.DeviceBootstrapSpec{Enabled: true},
				Service:   &laboratoryv1alpha1.DeviceService{Command: []string{"/usr/sbin/nginx"}, User: "nginx"},
			},
		}
		Expect(k8sClient.Create(ctx, dev)).To(Succeed())
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: dev.Name, Namespace: ns}}
		reconcileOnce() // state init
		reconcileOnce() // the first pod
	})

	It("bootstraps the first pod, and a pod restored from the saved state skips the bootstrap", func() {
		p := mustPod(1)
		Expect(p.Annotations).To(HaveKeyWithValue(names.AnnotationBootstrap, names.BootstrapRun))
		Expect(p.Annotations).To(HaveKeyWithValue(names.AnnotationBootstrapAttempt, "1"))
		Expect(p.Spec.Containers[0].Command).To(ContainElements("-bootstrap", "-attempt", "1"))
		Expect(p.Spec.Containers[0].Image).To(Equal(base))

		setRunning(p, false)
		reconcileOnce()
		Expect(getDevice().Status.Bootstrap.Phase).To(Equal(laboratoryv1alpha1.BootstrapPending))
		Expect(getDevice().Status.Ready).To(BeFalse())

		setRunning(mustPod(1), true)
		reconcileOnce()
		Expect(getDevice().Status.Bootstrap).To(Equal(&laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapDone, Attempts: 1}))

		// The pod ends later (not a bootstrap failure); its saved state is restored and not bootstrapped again.
		p = mustPod(1)
		setEnded(p, "", 137)
		reconcileOnce()
		Expect(getDevice().Status.Bootstrap.Phase).To(Equal(laboratoryv1alpha1.BootstrapDone), "the service ending is not a failed bootstrap")
		exitSnapshotDone(p, snap)
		reconcileOnce()
		clock = clock.Add(5 * time.Minute)
		reconcileOnce()
		p2 := mustPod(2)
		Expect(p2.Spec.Containers[0].Image).To(Equal(snap))
		Expect(p2.Annotations).To(HaveKeyWithValue(names.AnnotationBootstrap, names.BootstrapSkip))
		Expect(p2.Spec.Containers[0].Command).NotTo(ContainElement("-bootstrap"))
		for _, v := range p2.Spec.Volumes {
			Expect(v.Name).NotTo(Equal(names.BootstrapSecretVolume), "a restored pod must not receive the list")
		}
		Expect(slices.ContainsFunc(p2.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == names.BootstrapInitContainer })).To(BeFalse())
	})

	It("never restores the state of a filesystem whose bootstrap did not finish, and gives up after three attempts", func() {
		failAttempt(1, 1, false)
		Expect(getDevice().Status.Bootstrap).To(Equal(&laboratoryv1alpha1.DeviceBootstrapStatus{
			Phase: laboratoryv1alpha1.BootstrapPending, Attempts: 1, Reason: "the script exited with code 1"}))
		p2 := mustPod(2)
		Expect(p2.Spec.Containers[0].Image).To(Equal(base), "the half-bootstrapped snapshot must be discarded")
		Expect(p2.Annotations).To(HaveKeyWithValue(names.AnnotationBootstrapAttempt, "2"))
		Expect(reg.deleted).NotTo(BeEmpty())
		Expect(getDevice().Status.State.Image).To(BeEmpty())

		failAttempt(2, 2, false)
		Expect(mustPod(3).Annotations).To(HaveKeyWithValue(names.AnnotationBootstrapAttempt, "3"))

		failAttempt(3, 3, true)
		Expect(getDevice().Status.Bootstrap.Phase).To(Equal(laboratoryv1alpha1.BootstrapFailed))
		Expect(getDevice().Status.Bootstrap.Attempts).To(Equal(int32(3)))
		reconcileOnce()
		_, err := getPod(4)
		Expect(errors.IsNotFound(err)).To(BeTrue(), "a failed device is not restarted in a loop")

		// A reset to the original image lets it try again.
		d := getDevice()
		orig := d.DeepCopy()
		d.Spec.State.ResetToken = "r1"
		Expect(k8sClient.Patch(ctx, d, client.MergeFrom(orig))).To(Succeed())
		for range 4 {
			reconcileOnce()
			clock = clock.Add(5 * time.Minute)
		}
		Expect(getDevice().Status.Bootstrap.Phase).To(Equal(laboratoryv1alpha1.BootstrapPending))
		Expect(getDevice().Status.Bootstrap.Attempts).To(BeZero())
		p4 := mustPod(4)
		Expect(p4.Annotations).To(HaveKeyWithValue(names.AnnotationBootstrapAttempt, "1"))
	})

	It("does not make a pod when the service command is unknown", func() {
		dev2 := dev.DeepCopy()
		dev2.ResourceVersion = ""
		dev2.Name = "lab-db"
		dev2.Spec.Name = "db"
		dev2.Spec.Service = nil
		Expect(k8sClient.Create(ctx, dev2)).To(Succeed())
		req2 := reconcile.Request{NamespacedName: types.NamespacedName{Name: dev2.Name, Namespace: ns}}
		for range 3 {
			_, err := r.Reconcile(ctx, req2)
			Expect(err).NotTo(HaveOccurred())
		}
		var got laboratoryv1alpha1.Device
		Expect(k8sClient.Get(ctx, req2.NamespacedName, &got)).To(Succeed())
		Expect(got.Status.Bootstrap).NotTo(BeNil())
		Expect(got.Status.Bootstrap.Phase).To(Equal(laboratoryv1alpha1.BootstrapFailed))
		Expect(got.Status.Bootstrap.Reason).To(ContainSubstring("service command is unknown"))
		var pods corev1.PodList
		Expect(k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{names.LabelDevice: "db"})).To(Succeed())
		Expect(pods.Items).To(BeEmpty())
	})
})
