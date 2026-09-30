package laboratory

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// LaunchConfig is the launch pacing policy.
type LaunchConfig struct {
	// MaxInFlight is the largest number of labs provisioning at once; 0 admits without limit.
	MaxInFlight int
	// WaveTimeout is how long an admitted lab holds its slot if it is not Ready.
	WaveTimeout time.Duration
	// HeadroomPercent of the schedulable CPU and memory must stay free after an admission.
	HeadroomPercent int
	// ResourceCheck gates admission on free CPU and memory.
	ResourceCheck bool
	// Prepull pulls the images of a class onto the nodes before its first lab is admitted.
	Prepull bool
	// PrepullTimeout bounds the wait for a prepull; admission goes on after it.
	PrepullTimeout time.Duration
	// Tick is how often the queue is processed; zero means defaultLaunchTick.
	Tick time.Duration
	// StatusBudget is the largest number of queue status writes per tick; zero
	// means defaultStatusBudget. It keeps a long queue from flooding the API server.
	StatusBudget int
	// StatusInterval is the least time between two queue status writes of one lab;
	// zero means defaultStatusInterval.
	StatusInterval time.Duration
}

const (
	defaultLaunchTick     = 2 * time.Second
	defaultStatusBudget   = 50
	defaultStatusInterval = 10 * time.Second
	// recentAdmissionTTL covers the lag of the informer cache after an admission:
	// the lab still looks queued there, and must not be admitted twice.
	recentAdmissionTTL = 30 * time.Second
)

// Launcher admits queued Labs so that a burst of new labs does not overload the
// cluster. A new Lab is created at once and stays Queued; every tick the launcher
// admits labs in launch class order (then creation time) while
//   - fewer than MaxInFlight labs are provisioning,
//   - the images of the lab's class are on the nodes (prepull), and
//   - the cluster has free CPU and memory for the lab plus the headroom.
//
// It stops at the first lab that does not pass, so the order is never skipped
// (only a lab larger than the whole cluster steps aside). The launcher is the
// only writer of a Queued lab's status. It runs on the leader only.
type Launcher struct {
	client.Client
	Recorder record.EventRecorder
	Config   LaunchConfig
	// Defaults are the device resources a device without any gets; the same the
	// DeviceReconciler applies, so the resource check sees the real load.
	Defaults DeviceDefaults
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

	prepared  map[string]struct{}
	preparing string
	recent    map[types.UID]time.Time
	lastWrite map[types.UID]time.Time
}

// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/status,verbs=get;update;patch

// NeedLeaderElection makes the manager run the launcher on the leader only.
func (l *Launcher) NeedLeaderElection() bool { return true }

// Start runs the launcher until ctx ends.
func (l *Launcher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("launcher")
	tick := l.Config.Tick
	if tick <= 0 {
		tick = defaultLaunchTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := l.tick(ctx); err != nil {
				logger.Error(err, "launch tick")
			}
		}
	}
}

func (l *Launcher) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Launcher) namespace() string {
	if l.Namespace != "" {
		return l.Namespace
	}
	return names.SystemNamespace
}

// tick processes the queue once.
func (l *Launcher) tick(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("launcher")
	if l.prepared == nil {
		l.prepared = map[string]struct{}{}
		l.recent = map[types.UID]time.Time{}
		l.lastWrite = map[types.UID]time.Time{}
	}
	now := l.now()

	var list laboratoryv1alpha1.LabList
	if err := l.List(ctx, &list); err != nil {
		return fmt.Errorf("list labs: %w", err)
	}
	var queued, flight []*laboratoryv1alpha1.Lab
	seen := map[types.UID]bool{}
	for i := range list.Items {
		lab := &list.Items[i]
		seen[lab.UID] = true
		if labAdmitted(lab) {
			delete(l.recent, lab.UID)
			if labInFlight(lab, now, l.Config.WaveTimeout) {
				flight = append(flight, lab)
			}
			continue
		}
		if !lab.DeletionTimestamp.IsZero() {
			continue
		}
		if t, ok := l.recent[lab.UID]; ok && now.Sub(t) < recentAdmissionTTL {
			flight = append(flight, lab)
			continue
		}
		queued = append(queued, lab)
	}
	for uid := range l.lastWrite {
		if !seen[uid] {
			delete(l.lastWrite, uid)
		}
	}
	for uid := range l.recent {
		if !seen[uid] {
			delete(l.recent, uid)
		}
	}

	l.preparing = ""
	defer func() {
		if err := l.gcPrepull(ctx, l.preparing); err != nil {
			logger.Error(err, "clean up prepull daemonsets")
		}
	}()
	if len(queued) == 0 {
		return nil
	}

	order := orderQueue(queued)
	byClass := map[string][]*laboratoryv1alpha1.Lab{}
	for _, lab := range order {
		c := launchClassOf(lab)
		byClass[c] = append(byClass[c], lab)
	}

	unlimited := l.Config.MaxInFlight <= 0
	slots := l.Config.MaxInFlight - len(flight)
	var free *capacity
	blocker := ""
	reasons := map[types.UID]string{}
	admitted := map[types.UID]bool{}

admit:
	for _, lab := range order {
		class := launchClassOf(lab)
		ready, err := l.ensureClassPrepared(ctx, class, byClass[class], now)
		if err != nil {
			// A failed prepull only costs speed: go on without it.
			logger.Error(err, "prepull images", "class", class)
			l.prepared[class] = struct{}{}
			ready = true
		}
		if !unlimited && slots <= 0 {
			blocker = laboratoryv1alpha1.LaunchReasonInFlightLimit
			break
		}
		if !ready {
			blocker = laboratoryv1alpha1.LaunchReasonPreparingImages
			break
		}
		need := labNeed(lab, l.Defaults)
		if l.Config.ResourceCheck {
			if free == nil {
				c, err := l.loadCapacity(ctx, flight)
				if err != nil {
					return err
				}
				free = &c
			}
			switch free.check(need, l.Config.HeadroomPercent) {
			case fitWait:
				blocker = laboratoryv1alpha1.LaunchReasonInsufficientResources
				break admit
			case fitNoNodes:
				blocker = laboratoryv1alpha1.LaunchReasonNoSchedulableNodes
				break admit
			case fitNever:
				// Larger than the whole cluster: waiting cannot help, so the lab
				// steps aside instead of blocking every lab behind it.
				reasons[lab.UID] = laboratoryv1alpha1.LaunchReasonInsufficientResources
				continue
			}
			free.take(need)
		}
		if err := l.admit(ctx, lab, class, now); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			logger.Error(err, "admit lab", "lab", client.ObjectKeyFromObject(lab))
			break
		}
		admitted[lab.UID] = true
		l.recent[lab.UID] = now
		slots--
	}

	var remaining []*laboratoryv1alpha1.Lab
	for _, lab := range order {
		if !admitted[lab.UID] {
			remaining = append(remaining, lab)
		}
	}
	l.writeQueue(ctx, remaining, blocker, reasons, now)
	return nil
}

// admit lets a queued lab start provisioning. It patches only the launch fields,
// so the lab reconciler's own status writes are not overwritten.
func (l *Launcher) admit(ctx context.Context, lab *laboratoryv1alpha1.Lab, class string, now time.Time) error {
	orig := lab.DeepCopy()
	at := metav1.NewTime(now)
	lab.Status.Phase = laboratoryv1alpha1.PhaseProvisioning
	lab.Status.Launch = &laboratoryv1alpha1.LabLaunchStatus{Class: class, AdmittedAt: &at}
	if err := l.Status().Patch(ctx, lab, client.MergeFrom(orig)); err != nil {
		return err
	}
	if l.Recorder != nil {
		l.Recorder.Eventf(lab, corev1.EventTypeNormal, "Admitted", "admitted from the launch queue (class %s)", class)
	}
	return nil
}

// writeQueue publishes the place of each remaining lab. A lab that is not marked
// Queued yet goes first; after that only labs whose numbers changed are written,
// each at most once per StatusInterval and at most StatusBudget per tick, nearest
// the head first. The position of a long queue therefore lags by a few ticks.
func (l *Launcher) writeQueue(ctx context.Context, remaining []*laboratoryv1alpha1.Lab, blocker string,
	reasons map[types.UID]string, now time.Time) {
	logger := log.FromContext(ctx).WithName("launcher")
	budget := l.Config.StatusBudget
	if budget <= 0 {
		budget = defaultStatusBudget
	}
	interval := l.Config.StatusInterval
	if interval <= 0 {
		interval = defaultStatusInterval
	}
	wanted := make([]laboratoryv1alpha1.LabLaunchStatus, len(remaining))
	for i, lab := range remaining {
		reason := blocker
		if r, ok := reasons[lab.UID]; ok {
			reason = r
		}
		wanted[i] = laboratoryv1alpha1.LabLaunchStatus{
			Class:    launchClassOf(lab),
			Position: int32(i + 1),
			Length:   int32(len(remaining)),
			Reason:   reason,
		}
	}
	write := func(i int) {
		lab := remaining[i]
		orig := lab.DeepCopy()
		want := wanted[i]
		lab.Status.Phase = laboratoryv1alpha1.PhaseQueued
		lab.Status.Launch = &want
		labstatus.SetReady(&lab.Status.Conditions, lab.Generation, false, labstatus.ReasonQueued,
			"waiting in the launch queue")
		if err := l.Status().Patch(ctx, lab, client.MergeFrom(orig)); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "write queue status", "lab", client.ObjectKeyFromObject(lab))
			}
			return
		}
		l.lastWrite[lab.UID] = now
		budget--
	}
	for i, lab := range remaining {
		if budget <= 0 {
			return
		}
		if lab.Status.Phase != laboratoryv1alpha1.PhaseQueued {
			write(i)
		}
	}
	for i, lab := range remaining {
		if budget <= 0 {
			return
		}
		if lab.Status.Phase != laboratoryv1alpha1.PhaseQueued {
			continue // written above, or over budget; retried next tick
		}
		cur := lab.Status.Launch
		if cur != nil && cur.Class == wanted[i].Class && cur.Position == wanted[i].Position &&
			cur.Length == wanted[i].Length && cur.Reason == wanted[i].Reason {
			continue
		}
		if t, ok := l.lastWrite[lab.UID]; ok && now.Sub(t) < interval {
			continue
		}
		write(i)
	}
}

// loadCapacity snapshots the free resources and holds back what the labs already
// admitted will still request: their pods may not exist or be scheduled yet.
func (l *Launcher) loadCapacity(ctx context.Context, flight []*laboratoryv1alpha1.Lab) (capacity, error) {
	var nodes corev1.NodeList
	if err := l.List(ctx, &nodes); err != nil {
		return capacity{}, fmt.Errorf("list nodes: %w", err)
	}
	var pods corev1.PodList
	if err := l.List(ctx, &pods); err != nil {
		return capacity{}, fmt.Errorf("list pods: %w", err)
	}
	c := snapshotCapacity(nodes.Items, pods.Items, l.LabNodeSelector, l.LabTolerations)

	scheduled := map[string]amount{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if lab := p.Labels[names.LabelLab]; lab != "" {
			k := p.Namespace + "/" + lab
			scheduled[k] = scheduled[k].add(podRequests(p))
		}
	}
	var reserved amount
	for _, lab := range flight {
		reserved = reserved.add(labNeed(lab, l.Defaults).sub(scheduled[lab.Namespace+"/"+lab.Name]).floorZero())
	}
	c.take(reserved)
	return c, nil
}

// ensureClassPrepared makes sure the images of a class are on the nodes and
// reports whether admission of the class may start. It creates the prepull
// DaemonSet, and removes it once every scheduled pod holds its images or the
// timeout passed (a missing or unpullable image must not stop the queue). The
// DaemonSet's creation time is the clock, so a restart of the operator neither
// loses nor extends the wait. Done classes are remembered until restart.
func (l *Launcher) ensureClassPrepared(ctx context.Context, class string, labs []*laboratoryv1alpha1.Lab, now time.Time) (bool, error) {
	if _, ok := l.prepared[class]; ok {
		return true, nil
	}
	images := classImages(labs, l.Mirror)
	if !l.Config.Prepull || len(images) == 0 {
		l.prepared[class] = struct{}{}
		return true, nil
	}
	logger := log.FromContext(ctx).WithName("launcher")
	key := types.NamespacedName{Namespace: l.namespace(), Name: prepullName(class)}
	var ds appsv1.DaemonSet
	if err := l.Get(ctx, key, &ds); err != nil {
		if !errors.IsNotFound(err) {
			return false, err
		}
		want := buildPrepullDaemonSet(key.Namespace, class, images, l.ImagePullSecrets, l.LabNodeSelector, l.LabTolerations)
		if err := l.Create(ctx, want); err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		l.preparing = prepullKey(class)
		logger.Info("prepulling class images", "class", class, "images", len(images))
		return false, nil
	}
	var pods corev1.PodList
	if err := l.List(ctx, &pods, client.InNamespace(key.Namespace), client.MatchingLabels{prepullLabel: prepullKey(class)}); err != nil {
		return false, err
	}
	pulled, desired, done := prepullProgress(&ds, pods.Items)
	timeout := l.Config.PrepullTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if !done && now.Sub(ds.CreationTimestamp.Time) < timeout {
		l.preparing = prepullKey(class)
		return false, nil
	}
	if done {
		logger.Info("class images are on the nodes", "class", class, "nodes", desired)
	} else {
		logger.Info("image prepull timed out, admitting anyway", "class", class, "pulled", pulled, "nodes", desired)
	}
	l.prepared[class] = struct{}{}
	return true, nil
}

// gcPrepull deletes every prepull DaemonSet except the one of keepKey.
func (l *Launcher) gcPrepull(ctx context.Context, keepKey string) error {
	var list appsv1.DaemonSetList
	if err := l.List(ctx, &list, client.InNamespace(l.namespace()), client.HasLabels{prepullLabel}); err != nil {
		return err
	}
	for i := range list.Items {
		ds := &list.Items[i]
		if ds.Labels[prepullLabel] == keepKey || !ds.DeletionTimestamp.IsZero() {
			continue
		}
		if err := l.Delete(ctx, ds, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
