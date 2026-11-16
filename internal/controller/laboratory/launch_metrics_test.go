package laboratory

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func at(base time.Time, s int) time.Time { return base.Add(time.Duration(s) * time.Second) }

func cond(t corev1.PodConditionType, when time.Time) corev1.PodCondition {
	return corev1.PodCondition{Type: t, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(when)}
}

func TestLaunchStagesSplitTheStartOfAPod(t *testing.T) {
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	queued, dispatched := at(base, 0), at(base, 10)
	pod := &corev1.Pod{}
	pod.CreationTimestamp = metav1.NewTime(at(base, 11))
	pod.Status.Conditions = []corev1.PodCondition{
		cond(corev1.PodScheduled, at(base, 12)), cond(corev1.PodReadyToStartContainers, at(base, 15)), cond(corev1.PodInitialized, at(base, 25)),
		cond(corev1.ContainersReady, at(base, 40)), cond(corev1.PodReady, at(base, 41)),
	}
	got := launchStages(&queued, &dispatched, pod, at(base, 43))
	want := map[string]time.Duration{
		stageQueueWait: 10 * time.Second, stagePodCreate: time.Second, stageSchedule: time.Second, stageSandbox: 3 * time.Second,
		stageInit: 10 * time.Second, stageContainers: 15 * time.Second, stageReadyProbe: time.Second, stageDeviceReady: 2 * time.Second,
		stageTotal: 33 * time.Second,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %v want %v (all %v)", k, got[k], v, got)
		}
	}
}

// A cluster without PodReadyToStartContainers, a pod that is missing and edges out of order all leave stages out instead of faking them.
func TestLaunchStagesLeaveOutWhatIsUnknown(t *testing.T) {
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	dispatched := at(base, 10)
	pod := &corev1.Pod{}
	pod.CreationTimestamp = metav1.NewTime(at(base, 11))
	pod.Status.Conditions = []corev1.PodCondition{cond(corev1.PodScheduled, at(base, 12)), cond(corev1.PodInitialized, at(base, 20)), cond(corev1.PodReady, at(base, 5))}
	got := launchStages(nil, &dispatched, pod, at(base, 30))
	if _, ok := got[stageSandbox]; ok {
		t.Fatal("no sandbox stage without its condition")
	}
	if got[stageInit] != 8*time.Second {
		t.Fatalf("init runs from PodScheduled when there is no sandbox condition: %v", got)
	}
	if _, ok := got[stageQueueWait]; ok {
		t.Fatal("no queue wait without a queue time")
	}
	if _, ok := got[stageReadyProbe]; ok {
		t.Fatalf("a Ready before ContainersReady is not a duration: %v", got)
	}
	if only := launchStages(nil, &dispatched, nil, at(base, 30)); len(only) != 1 || only[stageTotal] != 20*time.Second {
		t.Fatalf("without a pod only the total is known: %v", only)
	}
}

// The scheduler records the stages once, on the Starting -> Started edge.
func TestSchedulerObservesLaunchStagesOnce(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 10
	f := newSchedFixture(t, cfg)
	f.addLab("m", "", nil, "web")
	count := func() uint64 { return sampleCount(t, deviceLaunchSeconds, "stage", stageTotal) }
	before := count()
	f.tick()
	f.wantStates("m/web=S")
	if count() != before {
		t.Fatal("nothing is observed while the pod is starting")
	}
	f.ready("m", "web")
	f.tick()
	f.wantStates("m/web=D")
	if count()-before != 1 {
		t.Fatalf("%d observations after the device became Ready, want 1", count()-before)
	}
	f.tick()
	if count()-before != 1 {
		t.Fatal("a Started device is not observed again")
	}
}

func TestSchedulerPublishesTheQueue(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 2
	f := newSchedFixture(t, cfg)
	f.addLab("a", "g1", nil, "web", "db")
	f.addLab("b", "g2", nil, "web")
	f.tick()
	if got := gaugeValue(schedulerSlotsMax); got != 2 {
		t.Fatalf("slots max %v", got)
	}
	if got := gaugeValue(schedulerSlotsUsed); got != 2 {
		t.Fatalf("slots used %v", got)
	}
	if got := gaugeValue(schedulerQueuedPods); got != 1 {
		t.Fatalf("queued pods %v", got)
	}
	if got := gaugeValue(schedulerQueuedGroups); got != 1 {
		t.Fatalf("queued groups %v", got)
	}
}

// Every launch metric is on the registry the endpoint serves, under its documented name, without a name-bearing label.
func TestLaunchMetricsAreRegistered(t *testing.T) {
	deviceLaunchSeconds.WithLabelValues(stageTotal).Observe(1)
	labReadySeconds.Observe(1)
	prepullWaitSeconds.WithLabelValues("done").Observe(1)
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, mf := range mfs {
		have[mf.GetName()] = true
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "lab", "pod", "device", "namespace", "name":
					if len(mf.GetName()) > 11 && mf.GetName()[:11] == "laboratory_" && mf.GetName() != "laboratory_vpn_lab_initiated_dropped_packets" {
						t.Fatalf("%s has the label %q", mf.GetName(), l.GetName())
					}
				}
			}
		}
	}
	for _, name := range []string{"laboratory_device_launch_stage_seconds", "laboratory_lab_ready_seconds", "laboratory_scheduler_slots_max",
		"laboratory_scheduler_slots_used", "laboratory_scheduler_queued_pods", "laboratory_scheduler_queued_objects",
		"laboratory_scheduler_queued_deploy_groups", "laboratory_scheduler_prepull_in_progress", "laboratory_scheduler_dispatched_pods_total",
		"laboratory_scheduler_tick_seconds", "laboratory_scheduler_prepull_wait_seconds"} {
		if !have[name] {
			t.Errorf("%s is not registered", name)
		}
	}
}

func sampleCount(t *testing.T, c prometheus.Collector, labels ...string) uint64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			ok := true
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, l := range m.GetLabel() {
					found = found || (l.GetName() == labels[i] && l.GetValue() == labels[i+1])
				}
				ok = ok && found
			}
			if ok && m.GetHistogram() != nil {
				n += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return n
}
