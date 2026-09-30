package laboratory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func TestRestartDelayGrowsAndCaps(t *testing.T) {
	want := []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := restartDelay(int32(i)); got != w {
			t.Errorf("streak %d: got %v want %v", i, got, w)
		}
	}
	if restartDelay(50) != maxRestartDelay || restartDelay(7) != maxRestartDelay {
		t.Errorf("delay must cap at %v", maxRestartDelay)
	}
}

func TestStableDeviceMAC(t *testing.T) {
	a := stableDeviceMAC("ns", "lab-web", "eth1")
	if a != stableDeviceMAC("ns", "lab-web", "eth1") {
		t.Fatal("MAC must be deterministic")
	}
	if a == stableDeviceMAC("ns", "lab-web", "eth2") || a == stableDeviceMAC("ns", "lab-db", "eth1") {
		t.Fatal("MAC must differ per interface and device")
	}
	if !strings.HasPrefix(a, "02:") || len(a) != 17 {
		t.Fatalf("want a locally administered unicast MAC, got %s", a)
	}
}

func TestNetworkAnnotationStableMAC(t *testing.T) {
	d := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-web", Namespace: "ns"},
		Spec: laboratoryv1alpha1.DeviceSpec{Interfaces: []laboratoryv1alpha1.InterfaceSpec{
			{Name: "eth1"},
			{Name: "eth2", MAC: "random"},
			{Name: "eth3", MAC: "aa:bb:cc:dd:ee:ff"},
		}},
	}
	if got := networkAnnotation(d, false); got != "eth1@,eth2@|random,eth3@|aa:bb:cc:dd:ee:ff" {
		t.Fatalf("Deployment mode must be unchanged, got %q", got)
	}
	got := networkAnnotation(d, true)
	want := fmt.Sprintf("eth1@|%s,eth2@|%s,eth3@|aa:bb:cc:dd:ee:ff",
		stableDeviceMAC("ns", "lab-web", "eth1"), stableDeviceMAC("ns", "lab-web", "eth2"))
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func stateTestLab(t *testing.T, objs ...client.Object) *LabReconciler {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(pruneScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}, &laboratoryv1alpha1.Device{}).
		Build()
	return &LabReconciler{Client: c, State: StatePolicy{Enabled: true, MaxLayers: 10, MaxSnapshotBytes: 1 << 29}}
}

func newLab(name string) *laboratoryv1alpha1.Lab {
	return &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
}

func TestStateModeIsFixedAtCreation(t *testing.T) {
	ctx := context.Background()

	// A lab created with the switch on is stamped persistent and its devices carry the policy.
	r := stateTestLab(t, newLab("new"))
	lab := newLab("new")
	if err := r.Get(ctx, client.ObjectKeyFromObject(lab), lab); err != nil {
		t.Fatal(err)
	}
	if updated, err := r.ensureModes(ctx, lab); err != nil || !updated {
		t.Fatalf("first reconcile must stamp the lab: updated=%v err=%v", updated, err)
	}
	if lab.Status.StatePersistence == nil || !*lab.Status.StatePersistence {
		t.Fatal("lab created with the switch on must be persistent")
	}
	if spec := r.deviceStateSpec(lab, laboratoryv1alpha1.DeviceTypeContainer); spec == nil || !spec.Enabled || spec.MaxLayers != 10 {
		t.Fatalf("container device must carry the policy, got %+v", spec)
	}
	if r.deviceStateSpec(lab, laboratoryv1alpha1.DeviceTypeHub) != nil {
		t.Fatal("a switch runs no container and needs no state")
	}

	// Flipping the switch off never changes the stamped lab.
	r.State.Enabled = false
	if updated, err := r.ensureModes(ctx, lab); err != nil || updated {
		t.Fatalf("a stamped lab is left alone: updated=%v err=%v", updated, err)
	}
	if !*lab.Status.StatePersistence {
		t.Fatal("mode changed after creation")
	}

	// A lab created with the switch off stays on Deployments after it is flipped on.
	off := newLab("off")
	r2 := stateTestLab(t, off)
	r2.State.Enabled = false
	if _, err := r2.ensureModes(ctx, off); err != nil {
		t.Fatal(err)
	}
	r2.State.Enabled = true
	if _, err := r2.ensureModes(ctx, off); err != nil {
		t.Fatal(err)
	}
	if *off.Status.StatePersistence || r2.deviceStateSpec(off, laboratoryv1alpha1.DeviceTypeContainer) != nil {
		t.Fatal("lab created with the switch off must stay on Deployments")
	}
}

func TestLabThatAlreadyHasDevicesIsNotSwitched(t *testing.T) {
	ctx := context.Background()
	legacy := newLab("legacy")
	dev := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{
		Name: "legacy-web", Namespace: "ns", Labels: map[string]string{names.LabelLab: "legacy"},
	}}
	r := stateTestLab(t, legacy, dev)
	if _, err := r.ensureModes(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Status.StatePersistence == nil || *legacy.Status.StatePersistence {
		t.Fatal("a lab that predates the feature keeps its Deployments even when the switch is on")
	}
}

func TestDeviceStateInfo(t *testing.T) {
	now := metav1.Now()
	d := &laboratoryv1alpha1.Device{
		Spec:   laboratoryv1alpha1.DeviceSpec{State: &laboratoryv1alpha1.DeviceStateSpec{Enabled: true}},
		Status: laboratoryv1alpha1.DeviceStatus{State: &laboratoryv1alpha1.DeviceStateStatus{SnapshotAt: &now, SizeBytes: 7, Warning: "quota", Rescue: true}},
	}
	info := deviceStateInfo(d)
	if info == nil || info.SizeBytes != 7 || info.QuotaWarning != "quota" || !info.Rescue || info.LastSnapshotAt == nil {
		t.Fatalf("got %+v", info)
	}
	if deviceStateInfo(&laboratoryv1alpha1.Device{}) != nil {
		t.Fatal("a Deployment device has no state info")
	}
}

type fakeRegistry struct{ deleted []string }

func (f *fakeRegistry) DeleteRepo(_ context.Context, repo string) error {
	f.deleted = append(f.deleted, repo)
	return nil
}

var _ = Describe("Device state persistence: bare Pod lifecycle", func() {
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
	patchDeviceState := func(mutate func(*laboratoryv1alpha1.DeviceStateStatus)) {
		d := getDevice()
		orig := d.DeepCopy()
		mutate(d.Status.State)
		Expect(k8sClient.Status().Patch(ctx, d, client.MergeFrom(orig))).To(Succeed())
	}
	patchSpecState := func(mutate func(*laboratoryv1alpha1.DeviceStateSpec)) {
		d := getDevice()
		orig := d.DeepCopy()
		mutate(d.Spec.State)
		Expect(k8sClient.Patch(ctx, d, client.MergeFrom(orig))).To(Succeed())
	}
	setRunning := func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.PodIP = "10.1.2.3"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	setEnded := func(p *corev1.Pod, ranFor time.Duration) {
		finished := clock
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "web", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, StartedAt: metav1.NewTime(finished.Add(-ranFor)), FinishedAt: metav1.NewTime(finished),
		}}}}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	exitSnapshotDone := func(p *corev1.Pod, image string) {
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) {
			s.ExitSnapshotPod = p.Name
			if image != "" {
				s.Image = image
			}
		})
	}
	startDevice := func() *corev1.Pod {
		reconcileOnce() // state init
		reconcileOnce() // pod
		return mustPod(1)
	}

	BeforeEach(func() {
		testNo++
		ns = fmt.Sprintf("state-%d", testNo)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		clock = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		reg = &fakeRegistry{}
		r = &DeviceReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			Registry:            reg,
			Now:                 func() time.Time { return clock },
			ExitSnapshotTimeout: 30 * time.Second,
		}
		dev = &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-web", Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{
				Type: laboratoryv1alpha1.DeviceTypeContainer, Name: "web", LabRef: "lab", Image: base,
				Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCP}}},
				State:      &laboratoryv1alpha1.DeviceStateSpec{Enabled: true, MaxLayers: 10},
			},
		}
		Expect(k8sClient.Create(ctx, dev)).To(Succeed())
		req = reconcile.Request{NamespacedName: types.NamespacedName{Name: dev.Name, Namespace: ns}}
	})

	It("runs the device as a bare Pod with a stable MAC, not as a Deployment", func() {
		p := startDevice()
		Expect(p.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(p.Spec.Containers[0].Image).To(Equal(base))
		Expect(metav1.IsControlledBy(p, getDevice())).To(BeTrue())
		Expect(p.Labels).To(HaveKeyWithValue(names.LabelLab, "lab"))
		Expect(p.Labels).To(HaveKeyWithValue(names.LabelDevice, "web"))
		Expect(p.Annotations[names.AnnotationNetworks]).To(Equal("eth1@|" + stableDeviceMAC(ns, dev.Name, "eth1")))
		Expect(p.Annotations[names.AnnotationStateEpoch]).To(Equal("0"))

		var dep appsv1.Deployment
		err := k8sClient.Get(ctx, req.NamespacedName, &dep)
		Expect(errors.IsNotFound(err)).To(BeTrue(), "persistence mode must not create a Deployment")

		reconcileOnce()
		Expect(getDevice().Status.State.Incarnation).To(Equal(int32(1)), "a second reconcile must not create another pod")

		setRunning(p)
		reconcileOnce()
		d := getDevice()
		Expect(d.Status.Ready).To(BeTrue())
		Expect(d.Status.PodName).To(Equal(p.Name))
		Expect(d.Status.PodIP).To(Equal("10.1.2.3"))
	})

	It("waits for the exit snapshot, then recreates the pod from the latest snapshot", func() {
		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) { s.Image = snap })

		p1 = mustPod(1)
		setEnded(p1, time.Hour)
		res := reconcileOnce()
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		_, err := getPod(1)
		Expect(err).NotTo(HaveOccurred(), "the ended pod must stay until the exit snapshot is done")
		Expect(getDevice().Status.Ready).To(BeFalse())

		exitSnapshotDone(p1, "localhost:5035/lab/x/lab/web@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
		reconcileOnce() // deletes the ended pod
		_, err = getPod(1)
		Expect(errors.IsNotFound(err)).To(BeTrue())
		reconcileOnce() // creates the next incarnation

		p2 := mustPod(2)
		Expect(p2.Spec.Containers[0].Image).To(Equal("localhost:5035/lab/x/lab/web@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"))
		Expect(p2.Annotations[names.AnnotationNetworks]).To(Equal(p1.Annotations[names.AnnotationNetworks]), "same MAC on every incarnation")
		st := getDevice().Status.State
		Expect(st.Incarnation).To(Equal(int32(2)))
		Expect(st.RestoredAt).NotTo(BeNil())
		Expect(st.StoppedAt).To(BeNil())
	})

	It("recreates from the last snapshot when the exit snapshot does not arrive in time", func() {
		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) { s.Image = snap })

		p1 = mustPod(1)
		setEnded(p1, time.Hour)
		reconcileOnce()
		_, err := getPod(1)
		Expect(err).NotTo(HaveOccurred())

		clock = clock.Add(31 * time.Second)
		reconcileOnce() // gives up waiting, deletes
		_, err = getPod(1)
		Expect(errors.IsNotFound(err)).To(BeTrue())
		reconcileOnce()
		Expect(mustPod(2).Spec.Containers[0].Image).To(Equal(snap))
	})

	It("backs off when the pod keeps dying right after it starts", func() {
		p1 := startDevice()
		setRunning(p1)
		p1 = mustPod(1)
		setEnded(p1, time.Second)
		exitSnapshotDone(p1, "")
		reconcileOnce() // marks stoppedAt
		reconcileOnce() // deletes, counts the crash
		Expect(getDevice().Status.State.CrashStreak).To(Equal(int32(1)))

		res := reconcileOnce() // next pod is held back by the back-off
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		_, err := getPod(2)
		Expect(errors.IsNotFound(err)).To(BeTrue(), "no immediate recreation in a crash loop")

		clock = clock.Add(3 * time.Second)
		reconcileOnce()
		mustPod(2)
	})

	It("recycles the pod into rescue mode and back, keeping the snapshot", func() {
		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) { s.Image = snap })

		patchSpecState(func(s *laboratoryv1alpha1.DeviceStateSpec) { s.Rescue = true })
		reconcileOnce() // the running pod no longer matches: deleted (exit snapshot follows)
		p1Gone, err := getPod(1)
		if err == nil {
			Expect(p1Gone.DeletionTimestamp).NotTo(BeNil())
		}
		exitSnapshotDone(p1, "")
		reconcileOnce()
		reconcileOnce()

		p2 := mustPod(2)
		Expect(p2.Spec.Containers[0].Image).To(Equal(snap))
		Expect(p2.Spec.Containers[0].Command).To(Equal([]string{"/bin/sh", "-c", "trap : TERM INT; while :; do sleep 3600 & wait $!; done"}))
		Expect(p2.Annotations[names.AnnotationStateRescue]).To(Equal("true"))
		setRunning(p2)
		reconcileOnce()
		Expect(getDevice().Status.State.Rescue).To(BeTrue())

		patchSpecState(func(s *laboratoryv1alpha1.DeviceStateSpec) { s.Rescue = false })
		reconcileOnce()
		exitSnapshotDone(p2, "")
		reconcileOnce()
		reconcileOnce()
		p3 := mustPod(3)
		Expect(p3.Spec.Containers[0].Command).To(BeEmpty(), "leaving rescue restores the image entrypoint")
		Expect(p3.Annotations).NotTo(HaveKey(names.AnnotationStateRescue))
		Expect(p3.Spec.Containers[0].Image).To(Equal(snap))
	})

	It("resets a device to its base image and drops the snapshots", func() {
		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) {
			s.Image, s.Layers, s.SizeBytes, s.Warning = snap, 2, 1234, "old warning"
		})

		patchSpecState(func(s *laboratoryv1alpha1.DeviceStateSpec) { s.ResetToken = "r1" })
		reconcileOnce() // stops the pod
		exitSnapshotDone(p1, "")
		reconcileOnce()
		reconcileOnce()

		Expect(reg.deleted).To(ContainElement(fmt.Sprintf("lab/%s/lab/web", ns)))
		st := getDevice().Status.State
		Expect(st.Epoch).To(Equal(int32(1)))
		Expect(st.ResetToken).To(Equal("r1"))
		Expect(st.Image).To(BeEmpty())
		Expect(st.SizeBytes).To(BeZero())
		Expect(st.Warning).To(BeEmpty())

		reconcileOnce()
		p2 := mustPod(2)
		Expect(p2.Spec.Containers[0].Image).To(Equal(base), "after a reset the pod starts from the base image")
		Expect(p2.Annotations[names.AnnotationStateEpoch]).To(Equal("1"))

		// The same token again changes nothing.
		reconcileOnce()
		Expect(getDevice().Status.State.Epoch).To(Equal(int32(1)))
	})

	It("holds a suspended device down and brings it back from its snapshot", func() {
		group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, group) })

		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) { s.Image = snap })

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name}, group)).To(Succeed())
		group.Spec.Suspended = true
		Expect(k8sClient.Update(ctx, group)).To(Succeed())
		reconcileOnce()
		exitSnapshotDone(p1, "")
		reconcileOnce()
		reconcileOnce()
		_, err := getPod(2)
		Expect(errors.IsNotFound(err)).To(BeTrue(), "a suspended device must not get a new pod")
		Expect(getDevice().Status.Ready).To(BeFalse())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name}, group)).To(Succeed())
		group.Spec.Suspended = false
		Expect(k8sClient.Update(ctx, group)).To(Succeed())
		reconcileOnce()
		Expect(mustPod(2).Spec.Containers[0].Image).To(Equal(snap))
	})

	It("warns, without falling back to the base image, when the snapshot image cannot be pulled", func() {
		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		patchDeviceState(func(s *laboratoryv1alpha1.DeviceStateStatus) { s.Image = snap })
		setEnded(mustPod(1), time.Hour)
		exitSnapshotDone(mustPod(1), "")
		reconcileOnce()
		reconcileOnce()
		p2 := mustPod(2)
		p2.Status.Phase = corev1.PodPending
		p2.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
		Expect(k8sClient.Status().Update(ctx, p2)).To(Succeed())
		reconcileOnce()
		st := getDevice().Status.State
		Expect(st.Warning).To(ContainSubstring("snapshot image unavailable"))
		Expect(mustPod(2).Spec.Containers[0].Image).To(Equal(snap))
		_, err := getPod(3)
		Expect(errors.IsNotFound(err)).To(BeTrue())

		p2 = mustPod(2)
		p2.Status.ContainerStatuses = nil
		setRunning(p2)
		reconcileOnce()
		Expect(getDevice().Status.State.Warning).To(BeEmpty())
	})

	It("leaves a device without state persistence on its Deployment", func() {
		plain := &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-db", Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{
				Type: laboratoryv1alpha1.DeviceTypeContainer, Name: "db", LabRef: "lab", Image: base,
			},
		}
		Expect(k8sClient.Create(ctx, plain)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: plain.Name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: plain.Name, Namespace: ns}, &dep)).To(Succeed())
	})
})

func TestThrottleStateInfo(t *testing.T) {
	t0 := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	t5 := metav1.NewTime(t0.Add(5 * time.Second))
	t20 := metav1.NewTime(t0.Add(20 * time.Second))
	old := []laboratoryv1alpha1.DeviceRef{{Name: "web", State: &laboratoryv1alpha1.DeviceStateInfo{LastSnapshotAt: &t0, SizeBytes: 1}}}

	cur := []laboratoryv1alpha1.DeviceRef{{Name: "web", State: &laboratoryv1alpha1.DeviceStateInfo{LastSnapshotAt: &t5, SizeBytes: 1}}}
	if !throttleStateInfo(old, cur) || !cur[0].State.LastSnapshotAt.Equal(&t0) {
		t.Fatal("a snapshot time less than 10s newer must not be republished")
	}
	cur = []laboratoryv1alpha1.DeviceRef{{Name: "web", State: &laboratoryv1alpha1.DeviceStateInfo{LastSnapshotAt: &t20, SizeBytes: 1}}}
	if throttleStateInfo(old, cur) || !cur[0].State.LastSnapshotAt.Equal(&t20) {
		t.Fatal("a snapshot time 10s or more newer is published")
	}
}
