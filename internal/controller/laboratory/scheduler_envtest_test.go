package laboratory

import (
	"fmt"
	"time"

	"github.com/cybericebox/laboratory/internal/devices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

var schedSpecNo int

var _ = Describe("Scheduler", func() {
	const (
		timeout  = 15 * time.Second
		interval = 100 * time.Millisecond
	)
	var (
		ns    string
		dr    *DeviceReconciler
		clock time.Time
	)

	BeforeEach(func() {
		schedSpecNo++
		ns = fmt.Sprintf("sched-%d", schedSpecNo)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		clock = time.Now()
		dr = &DeviceReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Scheduled: true,
			Defaults: DeviceDefaults{CPU: "100m", Memory: "100Mi"}, Now: func() time.Time { return clock },
		}
	})

	reconcileDevice := func(name string) {
		_, err := dr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	getDevice := func(name string) *laboratoryv1alpha1.Device {
		var d laboratoryv1alpha1.Device
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d)).To(Succeed())
		return &d
	}
	setScheduling := func(name string, state laboratoryv1alpha1.PodScheduleState) {
		d := getDevice(name)
		orig := d.DeepCopy()
		now := metav1.NewTime(clock)
		d.Status.Scheduling = &laboratoryv1alpha1.PodSchedule{State: state, QueuedAt: &now}
		ExpectWithOffset(1, k8sClient.Status().Patch(ctx, d, client.MergeFrom(orig))).To(Succeed())
	}
	deployment := func(name string) (*appsv1.Deployment, error) {
		var dep appsv1.Deployment
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep)
		return &dep, err
	}
	newDevice := func(name string, state *laboratoryv1alpha1.DeviceStateSpec) {
		ExpectWithOffset(1, k8sClient.Create(ctx, &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{
				Type: laboratoryv1alpha1.DeviceTypeContainer, Name: "web", LabRef: "lab", Image: "nginx:alpine", State: state,
			},
		})).To(Succeed())
	}

	Describe("the device gate", func() {
		It("holds a new device's Deployment until the scheduler dispatches it", func() {
			newDevice("lab-web", nil)
			reconcileDevice("lab-web")
			Expect(getDevice("lab-web").Status.Scheduling).NotTo(BeNil())
			Expect(getDevice("lab-web").Status.Scheduling.State).To(Equal(laboratoryv1alpha1.PodQueued))
			_, err := deployment("lab-web")
			Expect(errors.IsNotFound(err)).To(BeTrue(), "a queued device must not have a workload")

			setScheduling("lab-web", laboratoryv1alpha1.PodStarting)
			reconcileDevice("lab-web")
			dep, err := deployment("lab-web")
			Expect(err).NotTo(HaveOccurred())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		})

		It("starts a device at once when the scheduler is off", func() {
			dr.Scheduled = false
			newDevice("lab-web", nil)
			reconcileDevice("lab-web")
			_, err := deployment("lab-web")
			Expect(err).NotTo(HaveOccurred())
			Expect(getDevice("lab-web").Status.Scheduling).To(BeNil())
		})

		It("records a device that already runs as Started and leaves its workload alone", func() {
			dr.Scheduled = false
			newDevice("lab-web", nil)
			reconcileDevice("lab-web") // an old operator created the Deployment
			dr.Scheduled = true
			reconcileDevice("lab-web")
			Expect(getDevice("lab-web").Status.Scheduling.State).To(Equal(laboratoryv1alpha1.PodStarted))
			dep, err := deployment("lab-web")
			Expect(err).NotTo(HaveOccurred())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		})

		It("does the same for a snapshot-backed device that runs as a bare Pod", func() {
			newDevice("lab-web", &laboratoryv1alpha1.DeviceStateSpec{Enabled: true, MaxLayers: 10})
			podOf := func() (*corev1.Pod, error) {
				var p corev1.Pod
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "lab-web-1"}, &p)
				return &p, err
			}
			reconcileDevice("lab-web")
			Expect(getDevice("lab-web").Status.Scheduling.State).To(Equal(laboratoryv1alpha1.PodQueued))
			_, err := podOf()
			Expect(errors.IsNotFound(err)).To(BeTrue(), "a queued device must not have a pod")

			setScheduling("lab-web", laboratoryv1alpha1.PodStarting)
			reconcileDevice("lab-web")
			_, err = podOf()
			Expect(err).NotTo(HaveOccurred())

			// A pod that ran once and ended is a recovery: it is recreated with no slot.
			Expect(k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lab-web-1", Namespace: ns}})).To(Succeed())
		})
	})

	Describe("the flow", func() {
		It("brings up three groups with a dependency, and a failed pod", func() {
			newLab := func(name, group string, after string, devices ...string) {
				lab := &laboratoryv1alpha1.Lab{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{}, Annotations: map[string]string{}},
				}
				if group != "" {
					lab.Labels[names.LabelDeployGroup] = group
				}
				if after != "" {
					lab.Annotations[names.AnnotationDeployAfter] = after
				}
				for _, d := range devices {
					lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{
						Name: d, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:alpine",
					})
				}
				Expect(k8sClient.Create(ctx, lab)).To(Succeed())
			}
			labs := []string{"a", "b", "c", "i"}
			DeferCleanup(func() { // no controller runs the OVS finalizers in this suite
				for _, n := range labs {
					var devs laboratoryv1alpha1.DeviceList
					_ = k8sClient.List(ctx, &devs, client.InNamespace(ns), client.MatchingLabels{names.LabelLab: n})
					for i := range devs.Items {
						devs.Items[i].Finalizers = nil
						_ = k8sClient.Update(ctx, &devs.Items[i])
					}
					var l laboratoryv1alpha1.Lab
					if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &l) == nil {
						l.Finalizers = nil
						_ = k8sClient.Update(ctx, &l)
						_ = k8sClient.Delete(ctx, &l)
					}
				}
			})
			newLab("a", "g1", "", "web", "db")
			newLab("b", "g2", "g1", "web")
			newLab("c", "g3", "", "web")
			newLab("i", "", "", "web")

			// The lab reconciler materialises the devices; they all queue.
			Eventually(func() int {
				var devs laboratoryv1alpha1.DeviceList
				_ = k8sClient.List(ctx, &devs, client.InNamespace(ns))
				return len(devs.Items)
			}, timeout, interval).Should(Equal(5))
			for _, d := range []string{devices.Name("a", "web"), devices.Name("a", "db"), devices.Name("b", "web"), devices.Name("c", "web"), devices.Name("i", "web")} {
				reconcileDevice(d)
				Expect(getDevice(d).Status.Scheduling.State).To(Equal(laboratoryv1alpha1.PodQueued), d)
			}
			// The lab shows it waits: no pod has started.
			labPhase := func(n string) laboratoryv1alpha1.Phase {
				var l laboratoryv1alpha1.Lab
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &l)
				return l.Status.Phase
			}
			Eventually(func() laboratoryv1alpha1.Phase { return labPhase("b") }, timeout, interval).Should(Equal(laboratoryv1alpha1.PhaseQueued))

			sched := &Scheduler{
				Client:   k8sClient,
				Config:   SchedulerConfig{MaxPods: 2, StartupTimeout: 5 * time.Minute, RestartThreshold: 5, StatusInterval: time.Nanosecond},
				Defaults: DeviceDefaults{CPU: "100m", Memory: "100Mi"},
				Now:      func() time.Time { return clock },
			}
			tick := func() {
				clock = clock.Add(time.Second)
				ExpectWithOffset(1, sched.tick(ctx)).To(Succeed())
			}
			state := func(d string) laboratoryv1alpha1.PodScheduleState { return getDevice(d).Status.Scheduling.State }
			ready := func(d string) {
				dev := getDevice(d)
				dev.Status.Ready = true
				ExpectWithOffset(1, k8sClient.Status().Update(ctx, dev)).To(Succeed())
			}

			// Window of two pods: the first group, lab a, takes both slots.
			tick()
			Expect([]laboratoryv1alpha1.PodScheduleState{state(devices.Name("a", "web")), state(devices.Name("a", "db")), state(devices.Name("b", "web")), state(devices.Name("c", "web")), state(devices.Name("i", "web"))}).To(Equal(
				[]laboratoryv1alpha1.PodScheduleState{st, st, qd, qd, qd}))
			for _, d := range []string{devices.Name("a", "web"), devices.Name("a", "db"), devices.Name("b", "web"), devices.Name("c", "web"), devices.Name("i", "web")} {
				reconcileDevice(d)
			}
			for _, d := range []string{devices.Name("a", "web"), devices.Name("a", "db")} {
				dep, err := deployment(workloadName(getDevice(d)))
				Expect(err).NotTo(HaveOccurred(), d)
				Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			}
			for _, d := range []string{devices.Name("b", "web"), devices.Name("c", "web"), devices.Name("i", "web")} {
				_, err := deployment(workloadName(getDevice(d)))
				Expect(errors.IsNotFound(err)).To(BeTrue(), d+" must wait")
			}

			// One pod is Ready: group g1 has nothing left to dispatch, g2 waits for it,
			// so g3 takes the slot. The wait is visible on the lab.
			ready(devices.Name("a", "db"))
			tick()
			Expect(state(devices.Name("a", "db"))).To(Equal(sd))
			Expect(state(devices.Name("c", "web"))).To(Equal(st))
			Expect(state(devices.Name("b", "web"))).To(Equal(qd))
			Eventually(func() string {
				var l laboratoryv1alpha1.Lab
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b"}, &l)
				if l.Status.Scheduling == nil {
					return ""
				}
				return l.Status.Scheduling.Reason + "|" + l.Status.Scheduling.Message
			}, timeout, interval).Should(Equal("WaitingForGroup|waiting for group g1"))

			// g1 is complete once its last pod is Ready; then g2, and the independent lab last.
			ready(devices.Name("a", "web"))
			ready(devices.Name("c", "web"))
			tick()
			Expect(state(devices.Name("b", "web"))).To(Equal(st))
			tick()
			Expect(state(devices.Name("i", "web"))).To(Equal(st))

			// A pod that never becomes Ready is failed after the timeout, with a warning.
			clock = clock.Add(6 * time.Minute)
			tick()
			Expect(state(devices.Name("b", "web"))).To(Equal(laboratoryv1alpha1.PodFailed))
			Expect(state(devices.Name("i", "web"))).To(Equal(laboratoryv1alpha1.PodFailed))
			Eventually(func() *laboratoryv1alpha1.PodFailure {
				var l laboratoryv1alpha1.Lab
				_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "b"}, &l)
				for _, d := range l.Status.Devices {
					if d.Name == "web" {
						return d.Failure
					}
				}
				return nil
			}, timeout, interval).Should(And(Not(BeNil()), HaveField("Reason", laboratoryv1alpha1.FailureStartupTimeout)))
		})
	})
})
