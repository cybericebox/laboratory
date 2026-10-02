package laboratory

import (
	"context"
	"fmt"
	"github.com/cybericebox/laboratory/internal/devices"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/netattach"
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
	if got := networkAnnotation(d, false); got != `[{"iface":"eth1"},{"iface":"eth2","mac":"random"},{"iface":"eth3","mac":"aa:bb:cc:dd:ee:ff"}]` {
		t.Fatalf("Deployment mode must be unchanged, got %q", got)
	}
	got := networkAnnotation(d, true)
	want := fmt.Sprintf(`[{"iface":"eth1","mac":"%s"},{"iface":"eth2","mac":"%s"},{"iface":"eth3","mac":"aa:bb:cc:dd:ee:ff"}]`,
		stableDeviceMAC("ns", "lab-web", "eth1"), stableDeviceMAC("ns", "lab-web", "eth2"))
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNetworkAnnotationDropsInjectedInterfaces(t *testing.T) {
	d := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-web", Namespace: "ns"},
		Spec: laboratoryv1alpha1.DeviceSpec{Interfaces: []laboratoryv1alpha1.InterfaceSpec{
			{Name: "a@x,eth0@y|ff:ff:ff:ff:ff:ff"},
			{Name: "lo"},
			{Name: "eth1", MAC: "ff:ff:ff:ff:ff:ff"},
			{Name: "eth2", MAC: "01:00:5e:00:00:01"},
			{Name: "eth3"},
		}},
	}
	got := netattach.Parse(networkAnnotation(d, false))
	if len(got) != 1 || got[0].Iface != "eth3" {
		t.Fatalf("only the valid interface survives, got %+v", got)
	}
}

func stateTestLab(t *testing.T, objs ...client.Object) *LabReconciler {
	t.Helper()
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme) // the device code allocator lists Services
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}, &laboratoryv1alpha1.Device{}).
		Build()
	return &LabReconciler{Client: c, Scheme: scheme, State: StatePolicy{Enabled: true, MaxLayers: 10, WriteQuotaBytes: 1 << 29}}
}

func newLab(name string) *laboratoryv1alpha1.Lab {
	return &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
}

func persistent(debounce *metav1.Duration) laboratoryv1alpha1.DeviceTemplate {
	return laboratoryv1alpha1.DeviceTemplate{Type: laboratoryv1alpha1.DeviceTypeContainer, Persistence: &laboratoryv1alpha1.DevicePersistence{Enabled: true, Debounce: debounce}}
}

// Persistence is a per-device decision made at the Device's creation: one lab may mix a
// snapshot-backed device and a normal one.
func TestPersistenceIsPerDevice(t *testing.T) {
	r := stateTestLab(t, newLab("new"))
	plain := laboratoryv1alpha1.DeviceTemplate{Type: laboratoryv1alpha1.DeviceTypeContainer}
	off := laboratoryv1alpha1.DeviceTemplate{Type: laboratoryv1alpha1.DeviceTypeContainer, Persistence: &laboratoryv1alpha1.DevicePersistence{}}

	if spec := r.deviceStateSpec(nil, persistent(nil)); spec == nil || !spec.Enabled || spec.MaxLayers != 10 {
		t.Fatalf("a persistent device carries the policy: %+v", spec)
	}
	if r.deviceStateSpec(nil, plain) != nil || r.deviceStateSpec(nil, off) != nil {
		t.Fatal("a device that did not ask for persistence is a Deployment")
	}
	// The topology may set the debounce; the rest of the policy stays the platform's.
	d := metav1.Duration{Duration: 42 * time.Second}
	if custom := r.deviceStateSpec(nil, persistent(&d)); custom.Debounce.Duration != 42*time.Second || custom.MaxLayers != 10 || custom.WriteQuotaBytes != r.State.WriteQuotaBytes {
		t.Fatalf("custom debounce: %+v", custom)
	}
	hub := persistent(nil)
	hub.Type = laboratoryv1alpha1.DeviceTypeHub
	if r.deviceStateSpec(nil, hub) != nil {
		t.Fatal("a switch runs no container and needs no state")
	}
	// The tenant's policy: it must allow persistence, and its limits are capped by the platform's.
	tenantOf := func(allowed bool, wq string) *laboratoryv1alpha1.Tenant {
		return &laboratoryv1alpha1.Tenant{Spec: laboratoryv1alpha1.TenantSpec{Persistence: laboratoryv1alpha1.TenantPersistence{Allowed: allowed, WriteQuota: wq}}}
	}
	if r.deviceStateSpec(tenantOf(false, ""), persistent(nil)) != nil {
		t.Fatal("a tenant that does not allow persistence gets Deployments")
	}
	if spec := r.deviceStateSpec(tenantOf(true, "64Mi"), persistent(nil)); spec == nil || spec.WriteQuotaBytes != 64<<20 {
		t.Fatalf("the tenant's lower write quota applies: %+v", spec)
	}
	if spec := r.deviceStateSpec(tenantOf(true, "10Gi"), persistent(nil)); spec == nil || spec.WriteQuotaBytes != r.State.WriteQuotaBytes {
		t.Fatalf("the platform ceiling caps the tenant: %+v", spec)
	}
	// The platform not allowing persistence: nothing is snapshot-backed.
	r.State.Enabled = false
	if r.deviceStateSpec(nil, persistent(nil)) != nil || r.deviceStateSpec(tenantOf(true, ""), persistent(nil)) != nil {
		t.Fatal("persistence is allowed by the platform switch only")
	}
}

// A device keeps what was stamped at its creation, whatever the platform switch does later.
func TestExistingDevicesKeepTheirMode(t *testing.T) {
	r := stateTestLab(t, newLab("mix"))
	spec := r.deviceStateSpec(nil, persistent(nil))
	dev := &laboratoryv1alpha1.Device{Spec: laboratoryv1alpha1.DeviceSpec{State: spec}}
	plainDev := &laboratoryv1alpha1.Device{}
	r.State.Enabled = false
	if !deviceStateEnabled(dev) || deviceStateEnabled(plainDev) {
		t.Fatal("the stamped devices must not change when the switch is flipped off")
	}
	r.State.Enabled = true
	if !deviceStateEnabled(dev) || deviceStateEnabled(plainDev) {
		t.Fatal("nor when it is flipped on")
	}
}

// A mixed lab: the persistent device gets the snapshot policy at creation, the normal one does
// not, and flipping the platform switch later changes neither.
func TestMixedLabAndSwitchFlip(t *testing.T) {
	ctx := context.Background()
	lab := newLab("mix")
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", Persistence: &laboratoryv1alpha1.DevicePersistence{Enabled: true}},
		{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "pg"},
	}
	r := stateTestLab(t, lab)
	get := func(name string) *laboratoryv1alpha1.Device {
		t.Helper()
		var d laboratoryv1alpha1.Device
		if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &d); err != nil {
			t.Fatal(err)
		}
		return &d
	}
	if err := r.materializeDevices(ctx, lab, nil); err != nil {
		t.Fatal(err)
	}
	if !deviceStateEnabled(get(devices.Name("mix", "web"))) || deviceStateEnabled(get(devices.Name("mix", "db"))) {
		t.Fatalf("mixed modes: web=%+v db=%+v", get(devices.Name("mix", "web")).Spec.State, get(devices.Name("mix", "db")).Spec.State)
	}
	// The switch goes off, then on again: existing devices are untouched.
	for _, allowed := range []bool{false, true} {
		r.State.Enabled = allowed
		if err := r.materializeDevices(ctx, lab, nil); err != nil {
			t.Fatal(err)
		}
		if !deviceStateEnabled(get(devices.Name("mix", "web"))) || deviceStateEnabled(get(devices.Name("mix", "db"))) {
			t.Fatalf("the platform switch (%v) changed an existing device", allowed)
		}
	}
	// A device created while the switch is off is a Deployment even if it asks for persistence.
	lab2 := newLab("late")
	lab2.Spec.Devices = lab.Spec.Devices[:1]
	r2 := stateTestLab(t, lab2)
	r2.State.Enabled = false
	if err := r2.materializeDevices(ctx, lab2, nil); err != nil {
		t.Fatal(err)
	}
	var d laboratoryv1alpha1.Device
	if err := r2.Get(ctx, client.ObjectKey{Namespace: "ns", Name: devices.Name("late", "web")}, &d); err != nil || deviceStateEnabled(&d) {
		t.Fatalf("persistence is allowed by the platform switch only: %v %+v", err, d.Spec.State)
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

// podBlindClient lists no pods: a cache that has not yet seen the pod just created.
type podBlindClient struct{ client.Client }

func (c podBlindClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return nil
	}
	return c.Client.List(ctx, list, opts...)
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
		Expect(p.Annotations[names.AnnotationNetworks]).To(Equal(`[{"iface":"eth1","mac":"` + stableDeviceMAC(ns, dev.Name, "eth1") + `"}]`))
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

	It("does not start the device twice when the cache has not seen the pod it just created", func() {
		p1 := startDevice()
		Expect(p1.Name).To(Equal(dev.Name + "-1"))
		r.Client = podBlindClient{k8sClient}
		r.Reader = k8sClient
		res := reconcileOnce()
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		_, err := getPod(2)
		Expect(errors.IsNotFound(err)).To(BeTrue(), "the existing pod of the current incarnation must stop a second creation")
		Expect(getDevice().Status.State.Incarnation).To(Equal(int32(1)))
	})

	It("does not drop a newer pod when its cached status is behind", func() {
		p1 := startDevice()
		setRunning(p1)
		reconcileOnce()
		// The pod of incarnation 2 exists, but the status the reconcile reads still says 1.
		newer := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: dev.Name + "-2", Namespace: ns,
				Labels:      map[string]string{names.LabelLab: "lab", names.LabelDevice: "web"},
				Annotations: map[string]string{names.AnnotationStateIncarnation: "2"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: base}}},
		}
		Expect(controllerutil.SetControllerReference(getDevice(), newer, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, newer)).To(Succeed())
		res := reconcileOnce()
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		_, err := getPod(2)
		Expect(err).NotTo(HaveOccurred(), "a pod newer than the status must be left alone")
		_, err = getPod(1)
		Expect(err).NotTo(HaveOccurred())
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
		Expect(p2.Spec.Containers[0].Command).To(Equal([]string{"/bin/sh", "-c", "trap 'exit 0' TERM INT QUIT HUP USR1 USR2; while :; do sleep 1; done"}))
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

// A mixed lab on a real API server: the persistent device becomes a bare Pod, the normal one a
// Deployment, and flipping the platform switch changes neither.
var _ = Describe("Device state persistence: per-device mode", func() {
	ctx := context.Background()

	It("runs a persistent device as a bare Pod and a normal one as a Deployment, and keeps both when the switch flips", func() {
		ns := "mixed-mode"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		// The Lab object stays out of the API server: the suite's manager would reconcile it
		// with a platform that does not allow persistence.
		lab := &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Name: "mix", Namespace: ns, UID: "11111111-2222-3333-4444-555555555555"},
			Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
				{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", Persistence: &laboratoryv1alpha1.DevicePersistence{Enabled: true}},
				{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "postgres"},
			}},
		}
		lr := &LabReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), State: StatePolicy{Enabled: true, MaxLayers: 10, WriteQuotaBytes: 1 << 29}}
		dr := &DeviceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Registry: &fakeRegistry{}, Now: time.Now, ExitSnapshotTimeout: 30 * time.Second}

		settle := func() {
			Expect(lr.materializeDevices(ctx, lab, nil)).To(Succeed())
			for _, n := range []string{devices.Name("mix", "web"), devices.Name("mix", "db")} {
				for i := 0; i < 2; i++ {
					_, err := dr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: n, Namespace: ns}})
					Expect(err).NotTo(HaveOccurred())
				}
			}
		}
		expectMixed := func() {
			var web, db laboratoryv1alpha1.Device
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: devices.Name("mix", "web"), Namespace: ns}, &web)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: devices.Name("mix", "db"), Namespace: ns}, &db)).To(Succeed())
			Expect(deviceStateEnabled(&web)).To(BeTrue())
			Expect(deviceStateEnabled(&db)).To(BeFalse())

			var pods corev1.PodList
			Expect(k8sClient.List(ctx, &pods, client.InNamespace(ns))).To(Succeed())
			Expect(pods.Items).To(HaveLen(1), "only the persistent device is a bare Pod")
			Expect(pods.Items[0].Labels).To(HaveKeyWithValue(names.LabelDevice, "web"))
			var deps appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &deps, client.InNamespace(ns))).To(Succeed())
			Expect(deps.Items).To(HaveLen(1), "only the normal device is a Deployment")
			Expect(deps.Items[0].Labels).To(HaveKeyWithValue(names.LabelDevice, "db"))
		}

		settle()
		expectMixed()

		// A tenant that does not allow persistence gets a Deployment for the same topology.
		Expect(k8sClient.Create(ctx, &laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "locked"}})).To(Succeed())
		lockedLab := lab.DeepCopy()
		lockedLab.Name, lockedLab.UID = "locked", "99999999-2222-3333-4444-555555555555"
		lockedLab.Labels = map[string]string{names.LabelTenant: "locked"}
		Expect(lr.materializeDevices(ctx, lockedLab, nil)).To(Succeed())
		var lockedWeb laboratoryv1alpha1.Device
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: devices.Name("locked", "web"), Namespace: ns}, &lockedWeb)).To(Succeed())
		Expect(deviceStateEnabled(&lockedWeb)).To(BeFalse(), "the tenant does not allow persistence")
		Expect(lockedWeb.Labels).To(HaveKeyWithValue(names.LabelTenant, "locked"))

		// The platform stops allowing persistence, then allows it again: nothing changes.
		for _, allowed := range []bool{false, true} {
			lr.State.Enabled = allowed
			settle()
			expectMixed()
		}
	})
})
