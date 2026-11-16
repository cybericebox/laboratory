package laboratory

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Launch metrics: where the time of starting a lab goes. No lab, device, pod or group name is ever a label: the labels are the
// fixed stages and outcomes below, so the number of series does not grow with the event.

// launchBuckets cover half a second to ten minutes.
var launchBuckets = []float64{0.5, 1, 2, 3, 5, 8, 12, 20, 30, 45, 60, 90, 120, 180, 300, 600}

// Stages of a device pod, in the order they happen. See launchStages.
const (
	stageQueueWait   = "queue_wait"   // queued by the device reconciler -> dispatched by the scheduler (Starting)
	stagePodCreate   = "pod_create"   // dispatched -> the pod object exists
	stageSchedule    = "schedule"     // pod created -> PodScheduled (a node was chosen)
	stageSandbox     = "sandbox"      // PodScheduled -> PodReadyToStartContainers (sandbox and CNI done; Kubernetes 1.29 and later)
	stageInit        = "init"         // -> Initialized: init containers, so their image pulls (netconfig, probe install)
	stageContainers  = "containers"   // Initialized -> ContainersReady: image pull of the device image, start, first readiness
	stageReadyProbe  = "ready_probe"  // ContainersReady -> Ready
	stageDeviceReady = "device_ready" // pod Ready -> the scheduler saw the Device Ready (reconcile and tick lag)
	stageTotal       = "total"        // dispatched -> the scheduler saw the Device Ready
)

var (
	deviceLaunchSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_device_launch_stage_seconds",
		Help:    "Time of each stage of starting a device pod, from the scheduler's queue to the Device being Ready. Observed once per device when the scheduler records it as Started.",
		Buckets: launchBuckets,
	}, []string{"stage"})

	labReadySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "laboratory_lab_ready_seconds",
		Help:    "Time from a Lab starting to provision (its Ready condition went False, at its first reconcile or at a restart) to every device and connection being Ready.",
		Buckets: launchBuckets,
	})

	schedulerSlotsMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_scheduler_slots_max", Help: "Slots of the scheduler window (scheduler.maxPods; 0 = no limit).",
	})
	schedulerSlotsUsed = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_scheduler_slots_used", Help: "Pods dispatched and not yet Ready or failed (they hold a slot).",
	})
	schedulerQueuedPods = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_scheduler_queued_pods", Help: "Pods waiting in the scheduler queue.",
	})
	schedulerQueuedObjects = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_scheduler_queued_objects", Help: "Labs and groups waiting in the scheduler queue.",
	})
	schedulerQueuedGroups = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_scheduler_queued_deploy_groups", Help: "Distinct deploy groups that have a waiting object (a count, not a label).",
	})
	schedulerPrepulling = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_scheduler_prepull_in_progress", Help: "Prepull barriers (one per deploy group or topology class) the scheduler is waiting for.",
	})
	schedulerDispatched = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "laboratory_scheduler_dispatched_pods_total", Help: "Pods the scheduler dispatched.",
	})
	schedulerTickSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "laboratory_scheduler_tick_seconds", Help: "Duration of one scheduling pass.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	})
	prepullWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "laboratory_scheduler_prepull_wait_seconds",
		Help:    "How long a prepull barrier held the dispatch of a group, from the ImagePull request to the barrier opening. outcome: done, partial (some images failed) or timeout.",
		Buckets: launchBuckets,
	}, []string{"outcome"})
)

func init() {
	metrics.Registry.MustRegister(deviceLaunchSeconds, labReadySeconds, schedulerSlotsMax, schedulerSlotsUsed, schedulerQueuedPods,
		schedulerQueuedObjects, schedulerQueuedGroups, schedulerPrepulling, schedulerDispatched, schedulerTickSeconds, prepullWaitSeconds)
}

// launchStages splits the start of a device pod into its stages. queued and dispatched are from the Device's scheduling status, observed
// is when the scheduler saw the Device Ready; the rest comes from the newest pod's conditions. A stage whose edges are unknown or
// out of order is left out.
func launchStages(queued, dispatched *time.Time, pod *corev1.Pod, observed time.Time) map[string]time.Duration {
	out := map[string]time.Duration{}
	add := func(stage string, from, to time.Time) {
		if from.IsZero() || to.IsZero() || to.Before(from) {
			return
		}
		out[stage] = to.Sub(from)
	}
	var tQueued, tDispatched time.Time
	if queued != nil {
		tQueued = *queued
	}
	if dispatched != nil {
		tDispatched = *dispatched
	}
	add(stageQueueWait, tQueued, tDispatched)
	if pod == nil {
		add(stageTotal, tDispatched, observed)
		return out
	}
	cond := func(t corev1.PodConditionType) time.Time {
		for _, c := range pod.Status.Conditions {
			if c.Type == t && c.Status == corev1.ConditionTrue {
				return c.LastTransitionTime.Time
			}
		}
		return time.Time{}
	}
	created := pod.CreationTimestamp.Time
	scheduled, sandbox := cond(corev1.PodScheduled), cond(corev1.PodReadyToStartContainers)
	initialized, containers, ready := cond(corev1.PodInitialized), cond(corev1.ContainersReady), cond(corev1.PodReady)
	add(stagePodCreate, tDispatched, created)
	add(stageSchedule, created, scheduled)
	initFrom := scheduled
	if !sandbox.IsZero() {
		add(stageSandbox, scheduled, sandbox)
		initFrom = sandbox
	}
	add(stageInit, initFrom, initialized)
	add(stageContainers, initialized, containers)
	add(stageReadyProbe, containers, ready)
	add(stageDeviceReady, ready, observed)
	add(stageTotal, tDispatched, observed)
	return out
}

// observeLaunch records the stages of a device that became Ready.
func observeLaunch(queued, dispatched *time.Time, pod *corev1.Pod, observed time.Time) {
	for stage, d := range launchStages(queued, dispatched, pod, observed) {
		deviceLaunchSeconds.WithLabelValues(stage).Observe(d.Seconds())
	}
}
