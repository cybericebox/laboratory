package laboratory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/internal/tenant"
)

// SchedulerConfig is the scheduler policy.
type SchedulerConfig struct {
	// MaxPods is the largest number of pods starting at once; 0 sets no limit.
	MaxPods int
	// StartupTimeout is how long a dispatched pod may take to become Ready.
	StartupTimeout time.Duration
	// RestartThreshold is the restart count that fails a pod that is not Ready.
	RestartThreshold int
	// PlatformReservePercent of the schedulable CPU and memory (allocatable, so after the kubelet's reserve,
	// and after the requests of DaemonSets and the proxy) is never given to user labs.
	PlatformReservePercent int
	// PlatformReserveNode is an extra absolute reserve on every schedulable node.
	PlatformReserveNode corev1.ResourceList
	// PackingReservePercent of what is free on a node after the non-lab pods and the platform node reserve is never given to
	// labs. PackingReserveNode (CPU, memory), when non-zero, replaces the percent for that resource.
	PackingReservePercent int
	PackingReserveNode    corev1.ResourceList
	// ResourceCheck holds a pod back while no node has room for it.
	ResourceCheck bool
	// Prepull pulls the images of a group onto the nodes before its first pod starts.
	Prepull bool
	// PrepullTimeout bounds the wait for a prepull; dispatch goes on after it.
	PrepullTimeout time.Duration
	// Aging is how long a gang waits before the room it needs is kept for it (the oldest such gang only); zero: no aging.
	Aging time.Duration
	// Tick is how often the queue is processed; zero means defaultSchedulerTick.
	Tick time.Duration
	// StatusBudget is the largest number of queue status writes per tick; zero
	// means defaultStatusBudget. It keeps a long queue from flooding the API server.
	StatusBudget int
	// StatusInterval is the least time between two queue status writes of one
	// object; zero means defaultStatusInterval.
	StatusInterval time.Duration
	// UnreservedNotice is how long unreserved work that holds the room of active reservations is marked before it is deleted
	// (the reservations' ending notice; at least a minute).
	UnreservedNotice time.Duration
}

const (
	defaultSchedulerTick  = 2 * time.Second
	defaultStatusBudget   = 50
	defaultStatusInterval = 10 * time.Second
	// recentDispatchTTL covers the lag of the informer cache after a dispatch: the
	// pod still looks queued there and must not be dispatched twice.
	recentDispatchTTL = 30 * time.Second
)

// Scheduler dispatches the pods of Labs and LabGroups through a conveyor so a
// burst of objects does not start every pod at once. See planSchedule for the
// order and DEPLOY.md, "Scheduler". It is the only writer of the scheduling state
// of queued pods and runs on the leader.
type Scheduler struct {
	client.Client
	Reader   client.Reader
	Recorder record.EventRecorder
	Config   SchedulerConfig
	// Defaults are the device resources a device without any gets; the same the
	// DeviceReconciler applies, so the resource check sees the real load.
	Defaults DeviceDefaults
	// GroupPods are the resources of the VPN and gateway pods of a group; the scheduler counts them
	// against the node room and the tenant's quota like any pod.
	GroupPods grouppods.Config
	// LabNodeSelector and LabTolerations pick the nodes the prepull asks and the node
	// set of the resource check, like they shape the lab pods.
	LabNodeSelector map[string]string
	LabTolerations  []corev1.Toleration
	// Mirror rewrites image references of labs that use the image cache, so the
	// prepull warms the cache through the path the lab pods will use.
	Mirror imagecache.Rewriter
	// ImagesNamespace holds the credentials of a prepull request; empty means names.ImagesNamespace.
	ImagesNamespace string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// ReservationKindServed asks the API server, past any cache, whether it serves the Reservation kind, before work of a
	// reservation that is gone is deleted (ReservationKindServedBy); nil: not asked.
	ReservationKindServed func(context.Context) (bool, error)
	// NoReservationController: the operator runs without the ReservationReconciler (the CRD was missing at its start), so the
	// scheduler treats the kind as not served until a restart: no reservation is admitted whose window nobody would end.
	NoReservationController bool

	prepared map[string]struct{}
	// preparing holds the keys of the prepull requests in progress in this pass.
	preparing map[string]bool
	recent    map[string]time.Time
	lastWrite map[types.UID]time.Time
	// noReservations says the last list of Reservations found no such kind (the warning is logged once).
	noReservations bool
	// evictSeen is, for every marked target of the last pass with a known verdict, when this process first saw it so
	// (evictUnreserved), with the time of that pass; a pass without one leaves it empty, and a record older than seenMaxAge is
	// dropped, so the next marks get a new notice.
	evictSeen passRecord
	// evictRetry is when a due object whose deletion was refused or failed is tried again.
	evictRetry map[types.UID]time.Time
	// unsettledWarned holds the objects the plan left out of their gang (not set up in time) that got their warning event.
	unsettledWarned map[types.UID]bool
	// orphanSeen and orphanRetry are the same for the work of reservations that are gone (cleanOrphans).
	orphanSeen  passRecord
	orphanRetry map[types.UID]time.Time
	// usedAtPruned says the server dropped status.usedAt (an old Reservation CRD); logged once.
	usedAtPruned bool
}

// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=imagepulls,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs;labgroups;devices;tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/status;labgroups/status;devices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=reservations,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=reservations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs;labgroups,verbs=patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=tenants/status,verbs=get;update;patch

// NeedLeaderElection makes the manager run the scheduler on the leader only.
func (s *Scheduler) NeedLeaderElection() bool { return true }

// Start runs the scheduler until ctx ends.
func (s *Scheduler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("scheduler")
	tick := s.Config.Tick
	if tick <= 0 {
		tick = defaultSchedulerTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	// what an earlier run saw says nothing about the passes in between: every notice starts again
	s.evictSeen, s.orphanSeen = passRecord{}, passRecord{}
	// a replica that is no longer the leader reports no shortfall
	defer reservationShortfall.Reset()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.tick(ctx); err != nil {
				logger.Error(err, "scheduler tick")
			}
		}
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// imagesNamespace holds the credentials of the ImagePull requests.
func (s *Scheduler) imagesNamespace() string {
	if s.ImagesNamespace != "" {
		return s.ImagesNamespace
	}
	return names.ImagesNamespace
}

func devicePodKey(ns, lab, device string) string { return ns + "/" + lab + "/" + device }

// groupPodNeed is what a pod of the group requests: the size its spec carries, else the chart's group pod resources; nil for an
// unknown pod.
func (s *Scheduler) groupPodNeed(name string, g *laboratoryv1alpha1.LabGroup) corev1.ResourceList {
	switch name {
	case names.ComponentVPN:
		return s.GroupPods.VPNFor(g.Spec.VPN.Size).Requests
	case names.ComponentGateway:
		return s.GroupPods.GatewayFor(g.Spec.Gateway.Size).Requests
	}
	return nil
}

// groupPodNames are the pods a LabGroup runs itself.
func groupPodNames(lg *laboratoryv1alpha1.LabGroup) []string {
	var out []string
	if !lg.Spec.VPN.Disabled {
		out = append(out, names.ComponentVPN)
	}
	return append(out, names.ComponentGateway)
}

// deployAfter parses the deploy-after annotation.
func deployAfter(obj metav1.Object) []string {
	var out []string
	for _, k := range strings.Split(obj.GetAnnotations()[names.AnnotationDeployAfter], ",") {
		if k = strings.TrimSpace(k); k != "" {
			// The annotation holds the original keys; the group label holds names.DeployKey of them.
			out = append(out, names.DeployKey(k))
		}
	}
	return out
}

// deployPriority reads the explicit dispatch priority of an object: an integer annotation, 0 when absent or not a number.
func deployPriority(obj metav1.Object) int {
	n, err := strconv.Atoi(strings.TrimSpace(obj.GetAnnotations()[names.AnnotationDeployPriority]))
	if err != nil {
		return 0
	}
	return n
}

// topologyClass is a hash of the topology and images of a lab: independent labs
// built from one template share their image prepull through it.
func topologyClass(lab *laboratoryv1alpha1.Lab) string {
	lines := make([]string, 0, len(lab.Spec.Devices)+2)
	for _, d := range lab.Spec.Devices {
		lines = append(lines, fmt.Sprintf("device|%s|%s|%s", d.Name, d.Type, d.Image))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// classImages returns the sorted unique images of the container devices of
// labs, as the nodes will pull them: through the image cache for a lab that
// uses it.
func classImages(labs []*laboratoryv1alpha1.Lab, mirror imagecache.Rewriter) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range labs {
		if l.Spec.ImagePullSecret != "" {
			continue // pulled with its own credentials only: see objects
		}
		cached := l.Status.ImageCache != nil && *l.Status.ImageCache
		for _, d := range l.Spec.Devices {
			img := d.Image
			if cached {
				img = mirror.RewritePinned(img, l.Status.ImageDigests[d.Image])
			}
			if d.Type != laboratoryv1alpha1.DeviceTypeContainer || img == "" || seen[img] {
				continue
			}
			seen[img] = true
			out = append(out, img)
		}
	}
	sort.Strings(out)
	return out
}

// clusterView is the cluster as one tick sees it.
type clusterView struct {
	labs    []*laboratoryv1alpha1.Lab
	groups  []*laboratoryv1alpha1.LabGroup
	devices map[string]*laboratoryv1alpha1.Device // by devicePodKey
	pods    []*corev1.Pod
	// workloads are the Deployments of devices and group pods, by "namespace/name": what says why a pod was never created.
	workloads map[string]*appsv1.Deployment
	// podsOf maps a device key, and "ns/app" for a group pod, to its pods.
	podsOf    map[string][]*corev1.Pod
	suspended map[string]bool // namespace of a suspended group
	tenants   map[string]*laboratoryv1alpha1.Tenant
	// reservations are the Reservations of all tenants.
	reservations []laboratoryv1alpha1.Reservation
}

// load reads the cluster view of a pass: the labs past the cache, the rest from it; live reads all of it past the cache (the
// check before an eviction).
func (s *Scheduler) load(ctx context.Context, live bool) (*clusterView, error) {
	snap := &clusterView{devices: map[string]*laboratoryv1alpha1.Device{}, podsOf: map[string][]*corev1.Pod{}, suspended: map[string]bool{}, tenants: map[string]*laboratoryv1alpha1.Tenant{}}
	var r client.Reader = s.Client
	if live {
		r = s.directReader()
	}
	var labs laboratoryv1alpha1.LabList
	if err := s.directReader().List(ctx, &labs); err != nil {
		return nil, fmt.Errorf("list labs: %w", err)
	}
	for i := range labs.Items {
		snap.labs = append(snap.labs, &labs.Items[i])
	}
	var groups laboratoryv1alpha1.LabGroupList
	if err := r.List(ctx, &groups); err != nil {
		return nil, fmt.Errorf("list lab groups: %w", err)
	}
	for i := range groups.Items {
		g := &groups.Items[i]
		snap.groups = append(snap.groups, g)
		if g.Spec.Suspended || g.Spec.Lifecycle.IsStopped() {
			snap.suspended[laboratoryv1alpha1.LabGroupNamespaceOf(g)] = true
		}
	}
	var tenants laboratoryv1alpha1.TenantList
	if err := r.List(ctx, &tenants); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	for i := range tenants.Items {
		snap.tenants[tenants.Items[i].Name] = &tenants.Items[i]
	}
	var reservations laboratoryv1alpha1.ReservationList
	err := r.List(ctx, &reservations)
	if s.NoReservationController && err == nil {
		err = &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: laboratoryv1alpha1.SchemeGroupVersion.Group, Kind: "Reservation"}}
	}
	switch {
	case err == nil:
		snap.reservations = reservations.Items
		s.noReservations = s.noReservations && live
	case meta.IsNoMatchError(err) || errors.IsNotFound(err) || runtime.IsNotRegisteredError(err):
		// The Reservation CRD is not installed (an upgrade that skipped `kubectl apply` of the CRDs): the queue runs as if there
		// were no reservations, so unreserved labs go on and reserved ones wait with ReservationMissing. Said once until the CRD
		// is back.
		if !s.noReservations {
			log.FromContext(ctx).WithName("scheduler").Info("the Reservation CRD is not installed: scheduling as if there were no reservations", "error", err.Error())
			s.noReservations = true
		}
	default:
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	var devices laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &devices); err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	for i := range devices.Items {
		d := &devices.Items[i]
		snap.devices[devicePodKey(d.Namespace, d.Spec.LabRef, d.Spec.Name)] = d
	}
	snap.workloads = map[string]*appsv1.Deployment{}
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	for i := range deps.Items {
		snap.workloads[deps.Items[i].Namespace+"/"+deps.Items[i].Name] = &deps.Items[i]
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		snap.pods = append(snap.pods, p)
		if p.DeletionTimestamp != nil {
			continue
		}
		if lab, dev := p.Labels[names.LabelLab], p.Labels[names.LabelDevice]; lab != "" && dev != "" {
			k := devicePodKey(p.Namespace, lab, dev)
			snap.podsOf[k] = append(snap.podsOf[k], p)
		} else if app := systemComponentOf(p); app != "" {
			k := p.Namespace + "/" + app
			snap.podsOf[k] = append(snap.podsOf[k], p)
		}
	}
	return snap, nil
}

// setDevice records a new scheduling state on a Device (nil clears it).
func (s *Scheduler) setDevice(ctx context.Context, d *laboratoryv1alpha1.Device, ps *laboratoryv1alpha1.PodSchedule) error {
	orig := d.DeepCopy()
	d.Status.Scheduling = ps
	return s.Status().Patch(ctx, d, client.MergeFrom(orig))
}

// setGroupPod records the scheduling state of one pod of a LabGroup. The list is
// replaced as a whole, so the write is conditional on the version read.
func (s *Scheduler) setGroupPod(ctx context.Context, lg *laboratoryv1alpha1.LabGroup, name string, ps laboratoryv1alpha1.PodSchedule) error {
	orig := lg.DeepCopy()
	found := false
	for i := range lg.Status.Pods {
		if lg.Status.Pods[i].Name == name {
			lg.Status.Pods[i].PodSchedule = ps
			found = true
		}
	}
	if !found {
		lg.Status.Pods = append(lg.Status.Pods, laboratoryv1alpha1.NamedPodSchedule{Name: name, PodSchedule: ps})
	}
	return s.Status().Patch(ctx, lg, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// observe records what became of the pods already dispatched: Ready ones are
// Started, ones that took too long or crash too often are Failed. It
// changes the cached objects in place, so the plan sees the new states.
//
//nolint:gocyclo // one decision over many cases; splitting it would scatter the rule
func (s *Scheduler) observe(ctx context.Context, snap *clusterView, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	cfg := s.Config
	for _, d := range snap.devices {
		if d.DeletionTimestamp != nil || d.Spec.Type != laboratoryv1alpha1.DeviceTypeContainer || snap.suspended[d.Namespace] {
			continue
		}
		cur := d.Status.Scheduling
		if cur == nil {
			continue // not initialised by the device reconciler yet
		}
		if cur.State != laboratoryv1alpha1.PodStarting && (cur.State != laboratoryv1alpha1.PodFailed || !d.Status.Ready) {
			continue
		}
		ps := cur.DeepCopy()
		changed := false
		key := devicePodKey(d.Namespace, d.Spec.LabRef, d.Spec.Name)
		stamp := metav1.NewTime(now)
		switch {
		case (cur.State == laboratoryv1alpha1.PodStarting || cur.State == laboratoryv1alpha1.PodFailed) && d.Status.Ready:
			ps.State, ps.Failure, ps.StartedAt = laboratoryv1alpha1.PodStarted, nil, &stamp
			changed = true
		case cur.State == laboratoryv1alpha1.PodStarting:
			var extra int32
			if d.Status.State != nil && d.Status.State.Incarnation > 1 {
				extra = d.Status.State.Incarnation - 1
			}
			dispatched := now
			if cur.DispatchedAt != nil {
				dispatched = cur.DispatchedAt.Time
			}
			if failed, f := startupVerdict(now, dispatched, snap.podsOf[key], snap.workloads[d.Namespace+"/"+workloadName(d)], extra, cfg.StartupTimeout, cfg.RestartThreshold); failed {
				ps.State = laboratoryv1alpha1.PodFailed
				ps.Failure = &f
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := s.setDevice(ctx, d, ps); err != nil {
			logger.Error(err, "record pod state", "device", client.ObjectKeyFromObject(d))
			continue
		}
		if ps.State == laboratoryv1alpha1.PodFailed && s.Recorder != nil {
			s.Recorder.Eventf(d, corev1.EventTypeWarning, "PodStartFailed", "%s: %s", ps.Failure.Reason, ps.Failure.Message)
		}
	}
	for _, g := range snap.groups {
		if g.DeletionTimestamp != nil {
			continue
		}
		for _, entry := range g.Status.Pods {
			ps := entry.PodSchedule
			if ps.State != laboratoryv1alpha1.PodStarting && ps.State != laboratoryv1alpha1.PodFailed {
				continue
			}
			pods := snap.podsOf[laboratoryv1alpha1.LabGroupNamespaceOf(g)+"/"+entry.Name]
			ready := false
			for _, p := range pods {
				ready = ready || podReady(p)
			}
			stamp := metav1.NewTime(now)
			switch {
			case ready:
				ps.State, ps.Failure, ps.StartedAt = laboratoryv1alpha1.PodStarted, nil, &stamp
			case ps.State == laboratoryv1alpha1.PodStarting:
				dispatched := now
				if ps.DispatchedAt != nil {
					dispatched = ps.DispatchedAt.Time
				}
				failed, f := startupVerdict(now, dispatched, pods, snap.workloads[laboratoryv1alpha1.LabGroupNamespaceOf(g)+"/"+entry.Name], 0, cfg.StartupTimeout, cfg.RestartThreshold)
				if !failed {
					continue
				}
				ps.State, ps.Failure = laboratoryv1alpha1.PodFailed, &f
			default:
				continue
			}
			if err := s.setGroupPod(ctx, g, entry.Name, ps); err != nil {
				logger.Error(err, "record group pod state", "group", g.Name, "pod", entry.Name)
				break
			}
			if ps.State == laboratoryv1alpha1.PodFailed && s.Recorder != nil {
				s.Recorder.Eventf(g, corev1.EventTypeWarning, "PodStartFailed", "%s pod: %s: %s", entry.Name, ps.Failure.Reason, ps.Failure.Message)
			}
		}
	}
}

// noPodReason says why a dispatched workload has no pod. The Deployment carries the answer: its ReplicaFailure condition holds
// what the API server told its ReplicaSet (a quota, a Pod Security refusal, an admission webhook, a missing ServiceAccount).
func noPodReason(workload *appsv1.Deployment) (reason, message string) {
	switch {
	case workload == nil:
		return laboratoryv1alpha1.FailurePodNotCreated, "the workload was not created (its Deployment does not exist)"
	case workload.DeletionTimestamp != nil:
		return laboratoryv1alpha1.FailurePodNotCreated, "the workload is being deleted"
	case workload.Spec.Replicas != nil && *workload.Spec.Replicas == 0:
		return laboratoryv1alpha1.FailurePodNotCreated, "the workload is scaled to zero"
	}
	for _, c := range workload.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return laboratoryv1alpha1.FailurePodNotCreated, strings.TrimSpace("the pod could not be created: " + c.Reason + ": " + c.Message)
		}
	}
	return laboratoryv1alpha1.FailureStartupTimeout, "no pod was created"
}

// startupVerdict decides whether a dispatched pod that is not Ready is failed:
// it restarted too often, or it has taken longer than the timeout. The reason
// comes from what the node reports about the newest pod.
func startupVerdict(now, dispatched time.Time, pods []*corev1.Pod, workload *appsv1.Deployment, extraRestarts int32, timeout time.Duration, threshold int) (bool, laboratoryv1alpha1.PodFailure) {
	f := laboratoryv1alpha1.PodFailure{Reason: laboratoryv1alpha1.FailureStartupTimeout, Message: "the pod did not become Ready in time", RestartCount: extraRestarts}
	var newest *corev1.Pod
	for _, p := range pods {
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	if newest == nil {
		f.Reason, f.Message = noPodReason(workload)
	} else {
		for _, cs := range append(append([]corev1.ContainerStatus(nil), newest.Status.InitContainerStatuses...), newest.Status.ContainerStatuses...) {
			if cs.RestartCount > f.RestartCount {
				f.RestartCount = cs.RestartCount
			}
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
					f.Reason, f.Message = laboratoryv1alpha1.FailureImagePull, strings.TrimSpace(w.Reason+": "+w.Message)
				case "CrashLoopBackOff", "CreateContainerError", "RunContainerError":
					f.Reason, f.Message = laboratoryv1alpha1.FailureCrashLoop, strings.TrimSpace(w.Reason+": "+w.Message)
				}
			}
			if t := cs.LastTerminationState.Terminated; t != nil && f.Reason != laboratoryv1alpha1.FailureImagePull {
				msg := fmt.Sprintf("last exit code %d", t.ExitCode)
				if t.Reason != "" {
					msg = t.Reason + ", " + msg
				}
				if t.Message != "" {
					msg += ": " + t.Message
				}
				f.Reason, f.Message = laboratoryv1alpha1.FailureCrashLoop, msg
			}
		}
		if newest.Status.Phase == corev1.PodPending && f.Reason == laboratoryv1alpha1.FailureStartupTimeout {
			for _, c := range newest.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
					f.Reason, f.Message = laboratoryv1alpha1.FailureUnschedulable, strings.TrimSpace(c.Reason+": "+c.Message)
				}
			}
		}
	}
	if len(f.Message) > 500 {
		f.Message = f.Message[:500]
	}
	at := metav1.NewTime(now)
	f.At = &at
	switch {
	case threshold > 0 && int(f.RestartCount) >= threshold:
		if f.Reason == laboratoryv1alpha1.FailureStartupTimeout {
			f.Reason = laboratoryv1alpha1.FailureCrashLoop
		}
		return true, f
	case timeout > 0 && now.Sub(dispatched) >= timeout:
		return true, f
	}
	return false, f
}

// objects builds the plan input from the snapshot.
func (s *Scheduler) objects(snap *clusterView, now time.Time) []*schedObject {
	var out []*schedObject
	labsOfGroup := map[string][]*laboratoryv1alpha1.Lab{}
	for _, lab := range snap.labs {
		if lab.DeletionTimestamp == nil && !lab.Spec.Lifecycle.IsStopped() && !snap.suspended[lab.Namespace] {
			if g := lab.Labels[names.LabelDeployGroup]; g != "" {
				labsOfGroup[g] = append(labsOfGroup[g], lab)
			}
		}
	}
	imagesOfGroup := map[string][]string{}
	groupImages := func(group string) []string {
		if images, ok := imagesOfGroup[group]; ok {
			return images
		}
		images := classImages(labsOfGroup[group], s.Mirror)
		imagesOfGroup[group] = images
		return images
	}
	for _, lab := range snap.labs {
		if lab.DeletionTimestamp != nil || lab.Spec.Lifecycle.IsStopped() || snap.suspended[lab.Namespace] {
			continue
		}
		o := &schedObject{
			id: "lab/" + lab.Namespace + "/" + lab.Name, group: lab.Labels[names.LabelDeployGroup],
			after: deployAfter(lab), arrival: lab.CreationTimestamp.Time, priority: deployPriority(lab), ref: lab,
		}
		o.prepTenant = names.TenantOf(lab.Labels)
		o.reservation, o.labelled = names.ReservationOf(lab.Labels)
		switch {
		case lab.Spec.ImagePullSecret != "":
			// A lab with registry credentials of its own is not prepulled: the node-agents pull
			// with the tenant's credentials or none, and its images stay out of its group's
			// request (classImages), so it does not wait for that request either.
		case o.group != "":
			o.prepKey, o.images = prepClass(o.prepTenant, "g/"+o.group), groupImages(o.group)
		default:
			o.prepKey, o.images = prepClass(o.prepTenant, "l/"+topologyClass(lab)), classImages([]*laboratoryv1alpha1.Lab{lab}, s.Mirror)
		}
		for _, t := range lab.Spec.Devices {
			if p := s.devicePod(snap, lab, t, now); p != nil {
				o.pods = append(o.pods, p)
			}
		}
		out = append(out, o)
	}
	for _, g := range snap.groups {
		if g.DeletionTimestamp != nil || g.Spec.Lifecycle.IsStopped() {
			continue
		}
		o := &schedObject{
			id: "group/" + g.Name, group: g.Labels[names.LabelDeployGroup],
			after: deployAfter(g), arrival: g.CreationTimestamp.Time, priority: deployPriority(g), ref: g,
		}
		o.reservation, o.labelled = names.ReservationOf(g.Labels)
		if o.group != "" {
			o.prepTenant = names.TenantOf(g.Labels)
			o.prepKey, o.images = prepClass(o.prepTenant, "g/"+o.group), groupImages(o.group)
		}
		for _, name := range groupPodNames(g) {
			key := "group/" + g.Name + "/" + name
			p := &schedPod{key: key, name: name, kind: kindGroupPod, ref: name, tenant: names.TenantOf(g.Labels),
				lookup: laboratoryv1alpha1.LabGroupNamespaceOf(g) + "/" + name}
			if need := s.groupPodNeed(name, g); need != nil {
				p.need = amount{cpu: need.Cpu().MilliValue(), mem: need.Memory().Value()}
			}
			for _, e := range g.Status.Pods {
				if e.Name != name {
					continue
				}
				p.state = e.State
				if e.QueuedAt != nil {
					p.queuedAt = e.QueuedAt.Time
				}
				if s.recentDispatchPending(key, p.state, now) {
					p.state = laboratoryv1alpha1.PodStarting
				}
			}
			o.pods = append(o.pods, p)
		}
		out = append(out, o)
	}
	return out
}

// devicePod is the schedPod of a container device of a lab; nil for another type of device, and for a not dispatched pod of
// a failed lab (its graph did not validate: it gets no Devices), which is no member of its gang: nothing of it waits or is
// waited for, while what it already dispatched still counts.
func (s *Scheduler) devicePod(snap *clusterView, lab *laboratoryv1alpha1.Lab, t laboratoryv1alpha1.DeviceTemplate, now time.Time) *schedPod {
	if t.Type != laboratoryv1alpha1.DeviceTypeContainer {
		return nil
	}
	key := devicePodKey(lab.Namespace, lab.Name, t.Name)
	p := &schedPod{key: key, lookup: key, name: t.Name, kind: kindDevicePod, tenant: names.TenantOf(lab.Labels)}
	if need := guaranteedResources(t.Resources, s.Defaults); need != nil {
		p.need = amount{cpu: need.Cpu().MilliValue(), mem: need.Memory().Value()}
	}
	if d := snap.devices[key]; d != nil {
		p.ref = d
		if sc := d.Status.Scheduling; sc != nil {
			p.state = sc.State
			if sc.QueuedAt != nil {
				p.queuedAt = sc.QueuedAt.Time
			}
			if s.recentDispatchPending(key, p.state, now) {
				p.state = laboratoryv1alpha1.PodStarting
			}
		}
	}
	if lab.Status.Phase == laboratoryv1alpha1.PhaseFailed && !p.dispatched() {
		return nil
	}
	return p
}

// tick runs one scheduling pass.
func (s *Scheduler) tick(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("scheduler")
	if s.prepared == nil {
		s.prepared = map[string]struct{}{}
		s.recent = map[string]time.Time{}
		s.lastWrite = map[types.UID]time.Time{}
	}
	now := s.now()
	// Only a pass with a known capacity verdict carries the eviction record on (evictUnreserved), and only a pass that ran
	// shortly before this one gives it; the same for the work of gone reservations (cleanOrphans).
	evictSeen := s.evictSeen.recent(now, s.seenMaxAge())
	orphanSeen := s.orphanSeen.recent(now, s.seenMaxAge())
	s.evictSeen, s.orphanSeen = passRecord{}, passRecord{}
	// An expired dispatch no longer affects scheduling, even if its object was
	// deleted and never appears in the cache again. Prepared history is separate.
	for key, at := range s.recent {
		if now.Sub(at) >= recentDispatchTTL {
			delete(s.recent, key)
		}
	}
	snap, err := s.load(ctx, false)
	if err != nil {
		return err
	}
	s.observe(ctx, snap, now)
	objs := s.objects(snap, now)

	flight := 0
	for _, o := range objs {
		for _, p := range o.pods {
			if p.state == laboratoryv1alpha1.PodStarting {
				flight++
			} else {
				delete(s.recent, p.key)
			}
		}
	}

	s.preparing = map[string]bool{}
	defer func() {
		if err := s.gcPrepull(ctx, s.preparing); err != nil {
			logger.Error(err, "clean up prepull requests")
		}
	}()

	env := &clusterEnv{s: s, ctx: ctx, snap: snap, now: now, objs: objs}
	slots := s.Config.MaxPods - flight
	plan := planSchedule(objs, slots, s.Config.MaxPods <= 0, env)
	if env.err != nil {
		return env.err
	}

	stamp := metav1.NewTime(now)
	for _, p := range plan.dispatch {
		var err error
		switch p.kind {
		case kindDevicePod:
			d := p.ref.(*laboratoryv1alpha1.Device)
			var currentLab laboratoryv1alpha1.Lab
			if getErr := s.directReader().Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: d.Spec.LabRef}, &currentLab); getErr != nil || currentLab.Spec.Lifecycle.IsStopped() || !labStartPrepared(&currentLab) {
				continue
			}
			q := d.Status.Scheduling.DeepCopy()
			q.State, q.DispatchedAt, q.Failure = laboratoryv1alpha1.PodStarting, &stamp, nil
			err = s.setDevice(ctx, d, q)
		case kindGroupPod:
			err = s.dispatchGroupPod(ctx, snap, p, stamp)
		}
		if err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "dispatch pod", "pod", p.key)
			}
			continue
		}
		s.recent[p.key] = now
	}
	for _, f := range plan.failed {
		if f.pod.kind == kindGroupPod {
			if err := s.failGroupPod(ctx, snap, f, stamp); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "fail pod", "pod", f.pod.key)
			}
			continue
		}
		if d, ok := f.pod.ref.(*laboratoryv1alpha1.Device); ok {
			q := d.Status.Scheduling.DeepCopy()
			q.State = laboratoryv1alpha1.PodFailed
			reason := f.reason
			if reason == "" {
				reason = laboratoryv1alpha1.FailureDoesNotFit
			}
			q.Failure = &laboratoryv1alpha1.PodFailure{Reason: reason, Message: f.message, At: &stamp}
			if err := s.setDevice(ctx, d, q); err != nil {
				logger.Error(err, "fail pod", "pod", f.pod.key)
			}
		}
	}
	s.warnUnsettled(plan.unsettled)
	sf := env.shortfall(plan)
	s.writeStatuses(ctx, objs, plan, snap, env.fairness().usage, sf, now)
	if sf.known {
		reservationShortfall.WithLabelValues("cpu").Set(float64(sf.short.cpu) / 1000)
		reservationShortfall.WithLabelValues("memory").Set(float64(sf.short.mem))
		s.evictUnreserved(ctx, snap, sf.blockers, evictSeen, now)
	}
	s.cleanOrphans(ctx, snap, orphanSeen, now)
	return nil
}

// warnUnsettled gives every object the plan left out of its gang, because its pods were still not set up unsetSettleWindow
// after it was created, one Warning event; an object that is set up or gone may get one again later.
func (s *Scheduler) warnUnsettled(objs []*schedObject) {
	next := map[types.UID]bool{}
	for _, o := range objs {
		obj, ok := o.ref.(client.Object)
		if !ok {
			continue
		}
		next[obj.GetUID()] = true
		if s.unsettledWarned[obj.GetUID()] || s.Recorder == nil {
			continue
		}
		s.Recorder.Eventf(obj, corev1.EventTypeWarning, "PodsNotSetUp",
			"the pods of this object are still not set up %s after it was created: its deploy group goes on without it", unsetSettleWindow)
	}
	s.unsettledWarned = next
}

// failGroupPod records the failure of a LabGroup pod (VPN or gateway) the plan gave up on, so it is not planned again
// and again.
func (s *Scheduler) failGroupPod(ctx context.Context, snap *clusterView, f failedPod, stamp metav1.Time) error {
	name := f.pod.ref.(string)
	for _, g := range snap.groups {
		if "group/"+g.Name+"/"+name != f.pod.key {
			continue
		}
		var ps laboratoryv1alpha1.PodSchedule
		for _, e := range g.Status.Pods {
			if e.Name == name {
				ps = e.PodSchedule
			}
		}
		reason := f.reason
		if reason == "" {
			reason = laboratoryv1alpha1.FailureDoesNotFit
		}
		ps.State, ps.Failure = laboratoryv1alpha1.PodFailed, &laboratoryv1alpha1.PodFailure{Reason: reason, Message: f.message, At: &stamp}
		return s.setGroupPod(ctx, g, name, ps)
	}
	return nil
}

func (s *Scheduler) dispatchGroupPod(ctx context.Context, snap *clusterView, p *schedPod, stamp metav1.Time) error {
	name := p.ref.(string)
	for _, g := range snap.groups {
		if "group/"+g.Name+"/"+name != p.key {
			continue
		}
		var ps laboratoryv1alpha1.PodSchedule
		for _, e := range g.Status.Pods {
			if e.Name == name {
				ps = e.PodSchedule
			}
		}
		ps.State, ps.DispatchedAt, ps.Failure = laboratoryv1alpha1.PodStarting, &stamp, nil
		return s.setGroupPod(ctx, g, name, ps)
	}
	return errors.NewNotFound(schema.GroupResource{Group: laboratoryv1alpha1.SchemeGroupVersion.Group, Resource: "labgroups"}, p.key)
}

// clusterEnv answers the plan's questions from the cluster.
type clusterEnv struct {
	s        *Scheduler
	ctx      context.Context
	snap     *clusterView
	now      time.Time
	objs     []*schedObject
	snapshot *capacitySnapshot // the capacity of the nodes, read once per pass
	err      error
	// tenantUsed is what each tenant's dispatched pods request, built on first use.
	tenantUsed map[string]tenant.Totals
	// usage is the reservation ledger of the pass, built on first use.
	usage map[string]reservationUsage
	// live: the view was read past the cache to check an eviction; the plan is a dry run (no prepull is asked).
	live bool
	// heldPods are the placeholders of the pods of stopped labs that are gone, as the capacity snapshot counts them.
	heldPods []corev1.Pod
	// capacityUnknown: the capacity of this pass cannot be known (what stopped labs hold is unknown); asked once per pass.
	capacityUnknown bool
	// stoppedDone says stoppedPods, stoppedHeld and stoppedUnknown hold stoppedReservations of the view (computed once).
	stoppedDone    bool
	stoppedPods    []corev1.Pod
	stoppedHeld    map[string]tenant.Totals
	stoppedUnknown bool
}

// stopped is stoppedReservations of the pass's view, computed once; the map is shared, callers must not change it.
func (e *clusterEnv) stopped() ([]corev1.Pod, map[string]tenant.Totals, bool) {
	if !e.stoppedDone {
		e.stoppedPods, e.stoppedHeld, e.stoppedUnknown = e.s.stoppedReservations(e.snap)
		e.stoppedDone = true
	}
	return e.stoppedPods, e.stoppedHeld, e.stoppedUnknown
}

func (e *clusterEnv) prepared(o *schedObject) bool {
	if e.live {
		return false
	}
	ready, err := e.s.ensurePrepared(e.ctx, o.prepKey, o.prepTenant, o.images, e.now)
	if err != nil {
		// A failed prepull only costs speed: go on without it.
		log.FromContext(e.ctx).WithName("scheduler").Error(err, "prepull images", "key", o.prepKey)
		e.s.prepared[o.prepKey] = struct{}{}
		return true
	}
	return ready
}

func (e *clusterEnv) capacity() *capacitySnapshot {
	if e.snapshot != nil || e.err != nil || e.capacityUnknown {
		return e.snapshot
	}
	var r client.Reader = e.s.Client
	if e.live {
		r = e.s.directReader()
	}
	var nodes corev1.NodeList
	if err := r.List(e.ctx, &nodes); err != nil {
		e.err = fmt.Errorf("list nodes: %w", err)
		return nil
	}
	heldPods, _, unknown := e.stopped()
	if unknown {
		e.capacityUnknown = true
		return nil
	}
	pods := make([]corev1.Pod, 0, len(e.snap.pods)+len(heldPods))
	for _, p := range e.snap.pods {
		pods = append(pods, *p)
	}
	pods = append(pods, heldPods...)
	e.heldPods = heldPods
	c := snapshotCapacity(nodes.Items, pods, e.s.LabNodeSelector, e.s.LabTolerations, e.s.nodeReserve(), e.s.Config.PlatformReservePercent, e.s.packingReserve())
	e.snapshot = &c
	return e.snapshot
}

// rooms is where the placement simulation of a pass starts: the room for labs on every node less the lab pods bound to it.
// The plan places the pods that are dispatched but not on a node yet (nodeOf) itself.
func (e *clusterEnv) rooms() ([]nodeRoom, bool) {
	if !e.s.Config.ResourceCheck {
		return nil, false
	}
	c := e.capacity()
	if c == nil {
		return []nodeRoom{}, true
	}
	return c.freeRooms(), true
}

// nodeOf is the node the pod of a dispatched schedPod is bound to; empty: not on a node yet.
func (e *clusterEnv) nodeOf(p *schedPod) string {
	for _, pod := range e.snap.podsOf[p.lookup] {
		if pod.Spec.NodeName != "" {
			return pod.Spec.NodeName
		}
	}
	return ""
}

// emptyRooms is the room for labs on each node with no lab on it.
func (e *clusterEnv) emptyRooms() []nodeRoom {
	if !e.s.Config.ResourceCheck {
		return nil
	}
	c := e.capacity()
	if c == nil {
		return nil
	}
	return c.rooms()
}

// neverFits: no single node of the pool, empty, can give the pod its CPU and memory together.
func (e *clusterEnv) neverFits(p *schedPod) bool {
	if !e.s.Config.ResourceCheck || p.need == (amount{}) {
		return false
	}
	c := e.capacity()
	if c == nil || c.nodes == 0 {
		return false
	}
	return !c.fitsOneNode(p.need)
}

// noNodes: the resource check is on and no lab node is schedulable now.
func (e *clusterEnv) noNodes() bool {
	if !e.s.Config.ResourceCheck {
		return false
	}
	c := e.capacity()
	return c != nil && c.nodes == 0
}

// take counts a dispatched pod toward its tenant's quota for the rest of the pass.
func (e *clusterEnv) take(p *schedPod) {
	if e.tenantUsed != nil {
		e.tenantUsed[p.tenant] = e.tenantUsed[p.tenant].Add(tenant.Totals{CPU: p.need.cpu, Memory: p.need.mem})
	}
}

// tenantFits applies the tenant's CPU and memory quota to the sum of the requests of its
// dispatched pods, the VPN and gateway of its groups included (started, starting or failed: a failed
// pod's workload still runs).
func (e *clusterEnv) tenantFits(p *schedPod) bool {
	ten := e.snap.tenants[p.tenant]
	if ten == nil || ten.Spec.Quota == nil {
		return true
	}
	if e.tenantUsed == nil {
		_, held, unknown := e.stopped()
		if unknown {
			return false
		}
		e.tenantUsed = maps.Clone(held)
		for _, o := range e.objs {
			for _, q := range o.pods {
				if q.dispatched() {
					e.tenantUsed[q.tenant] = e.tenantUsed[q.tenant].Add(tenant.Totals{CPU: q.need.cpu, Memory: q.need.mem})
				}
			}
		}
	}
	var alloc tenant.Totals
	if tenant.NeedsAllocatable(ten.Spec.Quota) {
		c := e.capacity()
		if c == nil {
			return false
		}
		alloc = tenant.Totals{CPU: c.allocatable.cpu, Memory: c.allocatable.mem}
	}
	limits := tenant.ResolveQuota(ten.Spec.Quota, alloc)
	return limits.Fits(e.tenantUsed[p.tenant], tenant.Totals{CPU: p.need.cpu, Memory: p.need.mem})
}

// fairness is the reservation ledger, the clock and the aging threshold of the pass. The ledger counts the dispatched pods of
// the objects in the queue (Starting, Started or Failed: a failed pod's workload still runs); the pods of a stopped lab or of an
// object being deleted are not in it.
func (e *clusterEnv) fairness() fairness {
	if e.usage == nil {
		e.usage = reservationLedger(e.snap.reservations, ledgerPods(e.objs), e.now)
	}
	return fairness{usage: e.usage, now: e.now, aging: e.s.Config.Aging}
}

// stoppedHolds is the room the pods of stopped labs keep on each node: their pods still there and the held placeholders of
// those gone (stoppedReservations), as the capacity snapshot counts them.
func (e *clusterEnv) stoppedHolds() map[string]amount {
	if !e.s.Config.ResourceCheck {
		return nil
	}
	held, _, _ := e.stopped()
	stopped := map[string]bool{}
	for _, l := range e.snap.labs {
		if l.Spec.Lifecycle.IsStopped() {
			stopped[l.Namespace+"/"+l.Name] = true
		}
	}
	out := map[string]amount{}
	add := func(p *corev1.Pod) {
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			out[p.Spec.NodeName] = out[p.Spec.NodeName].add(podRequests(p))
		}
	}
	for _, p := range e.snap.pods {
		if l := p.Labels[names.LabelLab]; l != "" && stopped[p.Namespace+"/"+l] {
			add(p)
		}
	}
	for i := range held {
		add(&held[i])
	}
	return out
}

// tenantEverFits says whether the need fits the quota of the tenant with none of its pods running.
func (e *clusterEnv) tenantEverFits(p *schedPod) bool {
	ten := e.snap.tenants[p.tenant]
	if ten == nil || ten.Spec.Quota == nil {
		return true
	}
	var alloc tenant.Totals
	if tenant.NeedsAllocatable(ten.Spec.Quota) {
		c := e.capacity()
		if c == nil {
			return true
		}
		alloc = tenant.Totals{CPU: c.allocatable.cpu, Memory: c.allocatable.mem}
	}
	return tenant.ResolveQuota(ten.Spec.Quota, alloc).Fits(tenant.Totals{}, tenant.Totals{CPU: p.need.cpu, Memory: p.need.mem})
}

// writeStatuses publishes what each Reservation uses (status.used, status.groups, the status.usedAt heartbeat), the
// CapacityShortfall condition of the tenants and the queue place of each object. An object whose numbers changed is written at
// most once per StatusInterval and at most StatusBudget objects per tick, the reservations and tenants first (they are few),
// then the objects nearest the head, so a long queue lags by a few ticks instead of flooding the API server.
func (s *Scheduler) writeStatuses(ctx context.Context, objs []*schedObject, plan schedPlan, snap *clusterView, usage map[string]reservationUsage, sf shortfallReport, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	budget := s.Config.StatusBudget
	if budget <= 0 {
		budget = defaultStatusBudget
	}
	interval := s.Config.StatusInterval
	if interval <= 0 {
		interval = defaultStatusInterval
	}
	seen := map[types.UID]bool{}
	budget = s.writeReservations(ctx, snap.reservations, usage, now, interval, budget, seen)
	budget = s.writeTenants(ctx, snap, sf, now, interval, budget, seen)
	type item struct {
		o    *schedObject
		want laboratoryv1alpha1.SchedulingStatus
		pos  int32
	}
	var todo []item
	for _, o := range objs {
		st, waiting := plan.status[o.id]
		want := laboratoryv1alpha1.SchedulingStatus{Group: o.group, Pods: int32(len(o.pods))}
		pos := int32(1 << 30)
		if waiting {
			want.Position, want.Length, want.Reason, want.Message, want.Pending = st.Position, st.Length, st.Reason, st.Message, st.Pending
			pos = st.Position
		}
		var cur *laboratoryv1alpha1.SchedulingStatus
		var uid types.UID
		switch ref := o.ref.(type) {
		case *laboratoryv1alpha1.Lab:
			cur, uid = ref.Status.Scheduling, ref.UID
		case *laboratoryv1alpha1.LabGroup:
			cur, uid = ref.Status.Scheduling, ref.UID
		}
		seen[uid] = true
		if cur == nil && !waiting && want.Group == "" && want.Pods == 0 {
			continue
		}
		if cur != nil && *cur == want {
			continue
		}
		if t, ok := s.lastWrite[uid]; ok && now.Sub(t) < interval && cur != nil {
			continue
		}
		todo = append(todo, item{o, want, pos})
	}
	for uid := range s.lastWrite {
		if !seen[uid] {
			delete(s.lastWrite, uid)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].pos < todo[j].pos })
	for _, it := range todo {
		if budget <= 0 {
			return
		}
		want := it.want
		var err error
		var uid types.UID
		switch ref := it.o.ref.(type) {
		case *laboratoryv1alpha1.Lab:
			orig := ref.DeepCopy()
			ref.Status.Scheduling, uid = &want, ref.UID
			err = s.Status().Patch(ctx, ref, client.MergeFrom(orig))
		case *laboratoryv1alpha1.LabGroup:
			orig := ref.DeepCopy()
			ref.Status.Scheduling, uid = &want, ref.UID
			err = s.Status().Patch(ctx, ref, client.MergeFrom(orig))
		}
		if err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "write queue status", "object", it.o.id)
			}
			continue
		}
		s.lastWrite[uid] = now
		budget--
	}
}

// heartbeatBudget is the most status.usedAt heartbeats (writes of unchanged usage) per tick: a budget of their own, so they
// never take the queue status's, and the oldest go first.
const heartbeatBudget = 10

// writeReservations publishes what each Reservation uses (status.used, status.groups) when it changed, with status.usedAt,
// the time it was counted, within the per-object interval and the budget of the tick; it returns the budget left and marks
// the reservations seen. The unchanged usage of an open reservation is written again once usedAt is half the freshness bound
// old (usageHeartbeat), so the Exceeded deletion knows the usage is current: at most heartbeatBudget a tick, the oldest
// usedAt first, outside the budget.
func (s *Scheduler) writeReservations(ctx context.Context, reservations []laboratoryv1alpha1.Reservation, usage map[string]reservationUsage, now time.Time, interval time.Duration, budget int, seen map[types.UID]bool) int {
	logger := log.FromContext(ctx).WithName("scheduler")
	write := func(r *laboratoryv1alpha1.Reservation, u reservationUsage) bool {
		orig := r.DeepCopy()
		r.Status.Used = corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(u.used.cpu, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(u.used.mem, resource.BinarySI),
		}
		r.Status.Groups = u.groups
		at := metav1.NewTime(now)
		r.Status.UsedAt = &at
		if err := s.Status().Patch(ctx, r, client.MergeFrom(orig)); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "write reservation usage", "reservation", r.Name)
			}
			return false
		}
		if r.Status.UsedAt == nil && !s.usedAtPruned {
			// the answer of the server lost the field: its CRD is older than the operator
			logger.Info("the Reservation CRD prunes status.usedAt: apply the chart CRDs (the Exceeded deletion waits until then)")
			s.usedAtPruned = true
		}
		s.lastWrite[r.UID] = now
		return true
	}
	var beats []*laboratoryv1alpha1.Reservation
	for i := range reservations {
		r := &reservations[i]
		seen[r.UID] = true
		u := usage[r.Name]
		cur := r.Status
		if t, ok := s.lastWrite[r.UID]; ok && now.Sub(t) < interval {
			continue
		}
		if cur.Used != nil && cur.Used.Cpu().MilliValue() == u.used.cpu && cur.Used.Memory().Value() == u.used.mem && cur.Groups == u.groups {
			if reservationActive(r, now) && (cur.UsedAt == nil || now.Sub(cur.UsedAt.Time) >= usageHeartbeat(interval)) {
				beats = append(beats, r)
			}
			continue
		}
		if budget > 0 && write(r, u) {
			budget--
		}
	}
	sort.SliceStable(beats, func(i, j int) bool {
		a, b := beats[i].Status.UsedAt, beats[j].Status.UsedAt
		return a == nil && b != nil || a != nil && b != nil && a.Before(b)
	})
	for i, r := range beats {
		if i >= heartbeatBudget {
			break
		}
		write(r, usage[r.Name])
	}
	return budget
}

// ensurePrepared makes sure the images of a group are on the nodes and reports whether
// its first pod may start. It creates an ImagePull, which the node-agents answer by pulling
// the images through their container runtime (no tenant code runs), and removes it once
// every listed node holds its images or has given up on them, or the timeout passed (a
// missing or unpullable image must not stop the queue). The ImagePull's creation time is
// the clock, so a restart of the operator neither loses nor extends the wait. Done keys are
// remembered until restart.
func (s *Scheduler) ensurePrepared(ctx context.Context, key, tenantName string, images []string, now time.Time) (bool, error) {
	if key == "" {
		return true, nil
	}
	if _, ok := s.prepared[key]; ok {
		return true, nil
	}
	if s.preparing[prepullKey(key)] {
		return false, nil
	}
	if !s.Config.Prepull || len(images) == 0 {
		s.prepared[key] = struct{}{}
		return true, nil
	}
	logger := log.FromContext(ctx).WithName("scheduler")
	ipKey := types.NamespacedName{Name: prepullName(key)}
	var ip laboratoryv1alpha1.ImagePull
	if err := s.Get(ctx, ipKey, &ip); err != nil {
		if !errors.IsNotFound(err) {
			return false, err
		}
		nodes, err := s.prepullNodes(ctx)
		if err != nil {
			return false, err
		}
		want := buildImagePull(key, tenantName, images, nodes)
		want.Spec.PullSecret = s.writePullSecret(ctx, want, tenantName)
		if err := s.Create(ctx, want); err != nil && !errors.IsAlreadyExists(err) {
			s.deletePullSecret(ctx, want.Spec.PullSecret)
			return false, err
		}
		s.preparing[prepullKey(key)] = true
		logger.Info("prepulling images", "key", key, "images", len(images), "nodes", len(nodes))
		return false, nil
	}
	prog := imagePullProgress(&ip)
	timeout := s.Config.PrepullTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	created := ip.CreationTimestamp.Time
	if created.IsZero() {
		created = now // not stamped yet: it has just been created
	}
	if !prog.done && now.Sub(created) < timeout {
		s.preparing[prepullKey(key)] = true
		return false, nil
	}
	switch {
	case prog.done && len(prog.failed) > 0:
		logger.Info("some images could not be pulled; dispatching anyway, their pods fail on their own",
			"key", key, "failed", prog.failed, "nodes", prog.desired)
	case prog.done:
		logger.Info("images are on the nodes", "key", key, "nodes", prog.desired)
	default:
		logger.Info("image prepull timed out, dispatching anyway", "key", key, "pulled", prog.pulled, "nodes", prog.desired)
	}
	s.prepared[key] = struct{}{}
	return true, nil
}

// prepullNodes lists the nodes lab pods can run on, sorted by name.
func (s *Scheduler) prepullNodes(ctx context.Context) ([]string, error) {
	var nodes corev1.NodeList
	if err := s.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var out []string
	for i := range nodes.Items {
		if schedulableNode(&nodes.Items[i], s.LabNodeSelector, s.LabTolerations) {
			out = append(out, nodes.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// writePullSecret copies the tenant's registry credentials (the Secret its spec names, in
// the tenants namespace) into the images namespace, where the node-agents read them, and
// returns the copy's name; "" when the tenant has none. A tenant whose Secret cannot be read
// is pulled for anonymously: the pods that need the credentials fail on their own.
// The platform's own pull secrets are never used here.
func (s *Scheduler) writePullSecret(ctx context.Context, ip *laboratoryv1alpha1.ImagePull, tenantName string) string {
	logger := log.FromContext(ctx).WithName("scheduler")
	secret, err := tenantPullSecret(ctx, s, tenantName)
	if err != nil || secret == "" {
		if err != nil {
			logger.Error(err, "read the tenant", "tenant", tenantName)
		}
		return ""
	}
	var src corev1.Secret
	if err := s.Get(ctx, types.NamespacedName{Namespace: names.TenantsNamespace, Name: secret}, &src); err != nil || src.Type != corev1.SecretTypeDockerConfigJson {
		logger.Error(err, "the tenant's image pull secret is missing or not a registry secret; pulling anonymously", "tenant", tenantName, "secret", secret)
		return ""
	}
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: ip.Name, Namespace: s.imagesNamespace(),
			Labels: map[string]string{managedByLabel: managedByValue, prepullLabel: ip.Labels[prepullLabel]},
		},
		Type: src.Type,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: src.Data[corev1.DockerConfigJsonKey]},
	}
	if err := s.Create(ctx, dst); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "write the image pull credentials for the prepull", "tenant", tenantName)
			return ""
		}
		if err := s.Update(ctx, dst); err != nil {
			logger.Error(err, "refresh the image pull credentials for the prepull", "tenant", tenantName)
			return ""
		}
	}
	return dst.Name
}

func (s *Scheduler) deletePullSecret(ctx context.Context, name string) {
	if name == "" {
		return
	}
	_ = s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.imagesNamespace()}})
}

// gcPrepull deletes every ImagePull, and its credentials, except the ones still in progress.
func (s *Scheduler) gcPrepull(ctx context.Context, keep map[string]bool) error {
	var list laboratoryv1alpha1.ImagePullList
	if err := s.List(ctx, &list, client.HasLabels{prepullLabel}); err != nil {
		return err
	}
	for i := range list.Items {
		ip := &list.Items[i]
		if keep[ip.Labels[prepullLabel]] || !ip.DeletionTimestamp.IsZero() {
			continue
		}
		if err := s.Delete(ctx, ip); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	var secrets corev1.SecretList
	if err := s.List(ctx, &secrets, client.InNamespace(s.imagesNamespace()), client.HasLabels{prepullLabel}); err != nil {
		return err
	}
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		if keep[sec.Labels[prepullLabel]] || !sec.DeletionTimestamp.IsZero() {
			continue
		}
		if err := s.Delete(ctx, sec); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// packingReserve is the hidden packing reserve, taken per node.
func (s *Scheduler) packingReserve() nodecap.PackingReserve {
	return nodecap.PackingReserve{Percent: s.Config.PackingReservePercent, CPU: s.Config.PackingReserveNode.Cpu().MilliValue(), Memory: s.Config.PackingReserveNode.Memory().Value()}
}

// nodeReserve is the absolute platform reserve of every node.
func (s *Scheduler) nodeReserve() amount {
	return amount{cpu: s.Config.PlatformReserveNode.Cpu().MilliValue(), mem: s.Config.PlatformReserveNode.Memory().Value()}
}

// systemComponentOf is "vpn" or "gateway" for the group's own pods, "" for any other pod. LabelComponent decides; a pod made
// before the label is told by `app` (device pods, which carry the lab label, are handled before this is asked).
func systemComponentOf(p *corev1.Pod) string {
	if c := p.Labels[names.LabelComponent]; c != "" {
		if c == names.ComponentVPN || c == names.ComponentGateway {
			return c
		}
		return ""
	}
	if app := p.Labels[labelApp]; app == names.ComponentVPN || app == names.ComponentGateway {
		return app
	}
	return ""
}

func (s *Scheduler) directReader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

func (s *Scheduler) recentDispatchPending(key string, state laboratoryv1alpha1.PodScheduleState, now time.Time) bool {
	at, ok := s.recent[key]
	return ok && state == laboratoryv1alpha1.PodQueued && now.Sub(at) < recentDispatchTTL
}
