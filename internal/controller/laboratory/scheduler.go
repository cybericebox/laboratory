package laboratory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
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
	// ResourceCheck holds a pod back while no node has room for it.
	ResourceCheck bool
	// Prepull pulls the images of a group onto the nodes before its first pod starts.
	Prepull bool
	// PrepullTimeout bounds the wait for a prepull; dispatch goes on after it.
	PrepullTimeout time.Duration
	// Tick is how often the queue is processed; zero means defaultSchedulerTick.
	Tick time.Duration
	// StatusBudget is the largest number of queue status writes per tick; zero
	// means defaultStatusBudget. It keeps a long queue from flooding the API server.
	StatusBudget int
	// StatusInterval is the least time between two queue status writes of one
	// object; zero means defaultStatusInterval.
	StatusInterval time.Duration
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
	Recorder record.EventRecorder
	Config   SchedulerConfig
	// Defaults are the device resources a device without any gets; the same the
	// DeviceReconciler applies, so the resource check sees the real load.
	Defaults DeviceDefaults
	// GroupPods are the resources of the VPN and gateway pods of a group; the scheduler counts them
	// against the node room and the tenant's quota like any pod.
	GroupPods grouppods.Config
	// ImagePullSecrets, LabNodeSelector and LabTolerations shape the prepull pods
	// and the node set of the resource check like they shape the lab pods.
	ImagePullSecrets []string
	LabNodeSelector  map[string]string
	LabTolerations   []corev1.Toleration
	// Mirror rewrites image references of labs that use the image cache, so the
	// prepull warms the cache through the path the lab pods will use.
	Mirror imagecache.Rewriter
	// Namespace holds the prepull DaemonSets (and the pull secrets); the operator's.
	Namespace string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	prepared map[string]struct{}
	// preparing holds the keys of the prepull DaemonSets in progress in this pass.
	preparing map[string]bool
	recent    map[string]time.Time
	lastWrite map[types.UID]time.Time
}

// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs;labgroups;devices;tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/status;labgroups/status;devices/status,verbs=get;update;patch

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

func (s *Scheduler) namespace() string {
	if s.Namespace != "" {
		return s.Namespace
	}
	return names.SystemNamespace
}

func devicePodKey(ns, lab, device string) string { return ns + "/" + lab + "/" + device }

// groupPodNeed is what a group pod requests (the chart's group pod resources); nil for an unknown pod.
func (s *Scheduler) groupPodNeed(name string) corev1.ResourceList {
	switch name {
	case "vpn":
		return s.GroupPods.VPN().Requests
	case "gateway":
		return s.GroupPods.Gateway().Requests
	}
	return nil
}

// groupPodNames are the pods a LabGroup runs itself.
func groupPodNames(lg *laboratoryv1alpha1.LabGroup) []string {
	var out []string
	if !lg.Spec.VPN.Disabled {
		out = append(out, "vpn")
	}
	return append(out, "gateway")
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
	// podsOf maps a device key, and "ns/app" for a group pod, to its pods.
	podsOf    map[string][]*corev1.Pod
	suspended map[string]bool // namespace of a suspended group
	tenants   map[string]*laboratoryv1alpha1.Tenant
}

func (s *Scheduler) load(ctx context.Context) (*clusterView, error) {
	snap := &clusterView{devices: map[string]*laboratoryv1alpha1.Device{}, podsOf: map[string][]*corev1.Pod{}, suspended: map[string]bool{}, tenants: map[string]*laboratoryv1alpha1.Tenant{}}
	var labs laboratoryv1alpha1.LabList
	if err := s.List(ctx, &labs); err != nil {
		return nil, fmt.Errorf("list labs: %w", err)
	}
	for i := range labs.Items {
		snap.labs = append(snap.labs, &labs.Items[i])
	}
	var groups laboratoryv1alpha1.LabGroupList
	if err := s.List(ctx, &groups); err != nil {
		return nil, fmt.Errorf("list lab groups: %w", err)
	}
	for i := range groups.Items {
		g := &groups.Items[i]
		snap.groups = append(snap.groups, g)
		if g.Spec.Suspended {
			snap.suspended[laboratoryv1alpha1.LabGroupNamespace(g.Name)] = true
		}
	}
	var tenants laboratoryv1alpha1.TenantList
	if err := s.List(ctx, &tenants); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	for i := range tenants.Items {
		snap.tenants[tenants.Items[i].Name] = &tenants.Items[i]
	}
	var devices laboratoryv1alpha1.DeviceList
	if err := s.List(ctx, &devices); err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	for i := range devices.Items {
		d := &devices.Items[i]
		snap.devices[devicePodKey(d.Namespace, d.Spec.LabRef, d.Spec.Name)] = d
	}
	var pods corev1.PodList
	if err := s.List(ctx, &pods); err != nil {
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
		} else if app := p.Labels["app"]; app == "vpn" || app == "gateway" {
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
			if failed, f := startupVerdict(now, dispatched, snap.podsOf[key], extra, cfg.StartupTimeout, cfg.RestartThreshold); failed {
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
			pods := snap.podsOf[g.Name+"/"+entry.Name]
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
				failed, f := startupVerdict(now, dispatched, pods, 0, cfg.StartupTimeout, cfg.RestartThreshold)
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

// startupVerdict decides whether a dispatched pod that is not Ready is failed:
// it restarted too often, or it has taken longer than the timeout. The reason
// comes from what the node reports about the newest pod.
func startupVerdict(now, dispatched time.Time, pods []*corev1.Pod, extraRestarts int32, timeout time.Duration, threshold int) (bool, laboratoryv1alpha1.PodFailure) {
	f := laboratoryv1alpha1.PodFailure{Reason: laboratoryv1alpha1.FailureStartupTimeout, Message: "the pod did not become Ready in time", RestartCount: extraRestarts}
	var newest *corev1.Pod
	for _, p := range pods {
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	if newest == nil {
		f.Message = "no pod was created"
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
		if lab.DeletionTimestamp == nil && !snap.suspended[lab.Namespace] {
			if g := lab.Labels[names.LabelDeployGroup]; g != "" {
				labsOfGroup[g] = append(labsOfGroup[g], lab)
			}
		}
	}
	for _, lab := range snap.labs {
		if lab.DeletionTimestamp != nil || snap.suspended[lab.Namespace] {
			continue
		}
		o := &schedObject{
			id: "lab/" + lab.Namespace + "/" + lab.Name, group: lab.Labels[names.LabelDeployGroup],
			after: deployAfter(lab), arrival: lab.CreationTimestamp.Time, ref: lab,
		}
		if o.group != "" {
			o.prepKey, o.images = "g/"+o.group, classImages(labsOfGroup[o.group], s.Mirror)
		} else {
			o.prepKey, o.images = "l/"+topologyClass(lab), classImages([]*laboratoryv1alpha1.Lab{lab}, s.Mirror)
		}
		for _, t := range lab.Spec.Devices {
			if t.Type != laboratoryv1alpha1.DeviceTypeContainer {
				continue
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
					if at, ok := s.recent[key]; ok && p.state == laboratoryv1alpha1.PodQueued && now.Sub(at) < recentDispatchTTL {
						p.state = laboratoryv1alpha1.PodStarting
					}
				}
			}
			o.pods = append(o.pods, p)
		}
		out = append(out, o)
	}
	for _, g := range snap.groups {
		if g.DeletionTimestamp != nil {
			continue
		}
		o := &schedObject{
			id: "group/" + g.Name, group: g.Labels[names.LabelDeployGroup],
			after: deployAfter(g), arrival: g.CreationTimestamp.Time, ref: g,
		}
		if o.group != "" {
			o.prepKey, o.images = "g/"+o.group, classImages(labsOfGroup[o.group], s.Mirror)
		}
		for _, name := range groupPodNames(g) {
			key := "group/" + g.Name + "/" + name
			p := &schedPod{key: key, name: name, kind: kindGroupPod, ref: name, tenant: names.TenantOf(g.Labels),
				lookup: laboratoryv1alpha1.LabGroupNamespace(g.Name) + "/" + name}
			if need := s.groupPodNeed(name); need != nil {
				p.need = amount{cpu: need.Cpu().MilliValue(), mem: need.Memory().Value()}
			}
			for _, e := range g.Status.Pods {
				if e.Name != name {
					continue
				}
				p.state = e.State
				if at, ok := s.recent[key]; ok && p.state == laboratoryv1alpha1.PodQueued && now.Sub(at) < recentDispatchTTL {
					p.state = laboratoryv1alpha1.PodStarting
				}
			}
			o.pods = append(o.pods, p)
		}
		out = append(out, o)
	}
	return out
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
	snap, err := s.load(ctx)
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
			logger.Error(err, "clean up prepull daemonsets")
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
		if d, ok := f.pod.ref.(*laboratoryv1alpha1.Device); ok {
			q := d.Status.Scheduling.DeepCopy()
			q.State = laboratoryv1alpha1.PodFailed
			q.Failure = &laboratoryv1alpha1.PodFailure{Reason: laboratoryv1alpha1.FailureDoesNotFit, Message: f.message, At: &stamp}
			if err := s.setDevice(ctx, d, q); err != nil {
				logger.Error(err, "fail pod", "pod", f.pod.key)
			}
		}
	}
	s.writeStatuses(ctx, objs, plan, now)
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
	s    *Scheduler
	ctx  context.Context
	snap *clusterView
	now  time.Time
	objs []*schedObject
	free *capacity
	err  error
	// tenantUsed is what each tenant's dispatched pods request, built on first use.
	tenantUsed map[string]tenant.Totals
}

func (e *clusterEnv) prepared(o *schedObject) bool {
	ready, err := e.s.ensurePrepared(e.ctx, o.prepKey, o.images, e.now)
	if err != nil {
		// A failed prepull only costs speed: go on without it.
		log.FromContext(e.ctx).WithName("scheduler").Error(err, "prepull images", "key", o.prepKey)
		e.s.prepared[o.prepKey] = struct{}{}
		return true
	}
	return ready
}

func (e *clusterEnv) capacity() *capacity {
	if e.free != nil || e.err != nil {
		return e.free
	}
	var nodes corev1.NodeList
	if err := e.s.List(e.ctx, &nodes); err != nil {
		e.err = fmt.Errorf("list nodes: %w", err)
		return nil
	}
	pods := make([]corev1.Pod, 0, len(e.snap.pods))
	for _, p := range e.snap.pods {
		pods = append(pods, *p)
	}
	c := snapshotCapacity(nodes.Items, pods, e.s.LabNodeSelector, e.s.LabTolerations, e.s.nodeReserve())
	// A dispatched pod that is not on a node yet will still request its resources.
	var reserved amount
	for _, o := range e.objs {
		for _, p := range o.pods {
			if p.state != laboratoryv1alpha1.PodStarting {
				continue
			}
			onNode := false
			for _, pod := range e.snap.podsOf[p.lookup] {
				onNode = onNode || pod.Spec.NodeName != ""
			}
			if !onNode {
				reserved = reserved.add(p.need)
			}
		}
	}
	c.take(reserved)
	e.free = &c
	return e.free
}

func (e *clusterEnv) check(p *schedPod) fit {
	if !e.s.Config.ResourceCheck {
		return fitOK
	}
	c := e.capacity()
	if c == nil {
		return fitWait
	}
	return c.check(p.need, e.s.Config.PlatformReservePercent)
}

func (e *clusterEnv) take(p *schedPod) {
	if e.free != nil {
		e.free.take(p.need)
	}
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
		e.tenantUsed = map[string]tenant.Totals{}
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

// writeStatuses publishes the queue place of each object. An object whose
// numbers changed is written at most once per StatusInterval and at most
// StatusBudget objects per tick, nearest the head first, so a long queue lags by
// a few ticks instead of flooding the API server.
func (s *Scheduler) writeStatuses(ctx context.Context, objs []*schedObject, plan schedPlan, now time.Time) {
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

// ensurePrepared makes sure the images of a group are on the nodes and reports
// whether its first pod may start. It creates the prepull DaemonSet and removes
// it once every scheduled pod holds its images or the timeout passed (a missing or
// unpullable image must not stop the queue). The DaemonSet's creation time is the
// clock, so a restart of the operator neither loses nor extends the wait. Done
// keys are remembered until restart.
func (s *Scheduler) ensurePrepared(ctx context.Context, key string, images []string, now time.Time) (bool, error) {
	if key == "" {
		return true, nil
	}
	if _, ok := s.prepared[key]; ok {
		return true, nil
	}
	if !s.Config.Prepull || len(images) == 0 {
		s.prepared[key] = struct{}{}
		return true, nil
	}
	logger := log.FromContext(ctx).WithName("scheduler")
	dsKey := types.NamespacedName{Namespace: s.namespace(), Name: prepullName(key)}
	var ds appsv1.DaemonSet
	if err := s.Get(ctx, dsKey, &ds); err != nil {
		if !errors.IsNotFound(err) {
			return false, err
		}
		want := buildPrepullDaemonSet(dsKey.Namespace, key, images, s.ImagePullSecrets, s.LabNodeSelector, s.LabTolerations)
		if err := s.Create(ctx, want); err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		s.preparing[prepullKey(key)] = true
		logger.Info("prepulling images", "key", key, "images", len(images))
		return false, nil
	}
	var pods corev1.PodList
	if err := s.List(ctx, &pods, client.InNamespace(dsKey.Namespace), client.MatchingLabels{prepullLabel: prepullKey(key)}); err != nil {
		return false, err
	}
	prog := prepullProgress(&ds, pods.Items)
	timeout := s.Config.PrepullTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	created := ds.CreationTimestamp.Time
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

// gcPrepull deletes every prepull DaemonSet except the ones still in progress.
func (s *Scheduler) gcPrepull(ctx context.Context, keep map[string]bool) error {
	var list appsv1.DaemonSetList
	if err := s.List(ctx, &list, client.InNamespace(s.namespace()), client.HasLabels{prepullLabel}); err != nil {
		return err
	}
	for i := range list.Items {
		ds := &list.Items[i]
		if keep[ds.Labels[prepullLabel]] || !ds.DeletionTimestamp.IsZero() {
			continue
		}
		if err := s.Delete(ctx, ds, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// nodeReserve is the absolute platform reserve of every node.
func (s *Scheduler) nodeReserve() amount {
	return amount{cpu: s.Config.PlatformReserveNode.Cpu().MilliValue(), mem: s.Config.PlatformReserveNode.Memory().Value()}
}
