package laboratory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/names"
)

// States of GroupPodSizeStatus.
const (
	podSizeApplied  = "Applied"
	podSizeLimited  = "Limited"
	podSizePending  = "Pending"
	podSizeFixed    = "Fixed"
	podSizeRecreate = "Recreate"
)

// GroupAutosizer sizes the VPN and gateway pods of every group in place, with the load of the group (see grouppods.Autosize for
// the formula and the hysteresis). It never restarts a pod: a resize the node cannot give is left as it is and the reason goes to
// the group's status and its events. Memory is raised as request = limit, so the kubelet itself admits it only when the node has
// room (otherwise the resize stays Deferred) and a pod never uses more than it requested; the CPU request stays and only the CPU
// limit follows the load. The room of the node is checked before a memory growth, and a growth larger than the room is cut to it.
type GroupAutosizer struct {
	Client    client.Client
	Reader    client.Reader
	GroupPods grouppods.Config
	Recorder  record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// resize applies the limits to the live pod (the pods/resize subresource); nil means the API server.
	resize func(ctx context.Context, pod *corev1.Pod, container string, cpuLimitMillicores, memoryBytes int64) error
	// nodeFree is the CPU (millicores) and memory (bytes) a node has left above the requests of its pods; nil means the API server.
	nodeFree func(ctx context.Context, node string) (cpu, memory int64, err error)

	lowerSince map[string]time.Time
}

// NeedLeaderElection makes the sizer run on the elected operator only.
func (a *GroupAutosizer) NeedLeaderElection() bool { return true }

// Start runs the sweep until ctx ends.
func (a *GroupAutosizer) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("group-autosizer")
	interval := a.GroupPods.Autosize.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.Sweep(ctx); err != nil {
			log.Error(err, "autosize sweep")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (a *GroupAutosizer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *GroupAutosizer) reader() client.Reader {
	if a.Reader != nil {
		return a.Reader
	}
	return a.Client
}

// Sweep looks at every group with a MaxSize once.
func (a *GroupAutosizer) Sweep(ctx context.Context) error {
	var groups laboratoryv1alpha1.LabGroupList
	if err := a.Client.List(ctx, &groups); err != nil {
		return err
	}
	if a.lowerSince == nil {
		a.lowerSince = map[string]time.Time{}
	}
	var firstErr error
	for i := range groups.Items {
		lg := &groups.Items[i]
		if lg.DeletionTimestamp != nil || lg.Status.Namespace == "" || lg.Spec.Suspended {
			continue
		}
		if err := a.SizeGroup(ctx, lg); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("group %s: %w", lg.Name, err)
		}
	}
	return firstErr
}

// SizeGroup does one pass over the pods of one group.
func (a *GroupAutosizer) SizeGroup(ctx context.Context, lg *laboratoryv1alpha1.LabGroup) error {
	if a.lowerSince == nil {
		a.lowerSince = map[string]time.Time{}
	}
	var results []laboratoryv1alpha1.GroupPodSizeStatus
	for _, component := range []string{names.ComponentVPN, names.ComponentGateway} {
		if component == names.ComponentVPN && lg.Spec.VPN.Disabled {
			continue
		}
		st, err := a.sizePod(ctx, lg, component)
		if err != nil {
			return err
		}
		if st != nil {
			results = append(results, *st)
		}
	}
	return a.publish(ctx, lg, results)
}

func (a *GroupAutosizer) units(ctx context.Context, ns, component string) (int, error) {
	if component == names.ComponentVPN {
		var clients laboratoryv1alpha1.LabGroupClientList
		if err := a.Client.List(ctx, &clients, client.InNamespace(ns)); err != nil {
			return 0, err
		}
		n := 0
		for i := range clients.Items {
			if clients.Items[i].DeletionTimestamp == nil {
				n++
			}
		}
		return n, nil
	}
	var labs laboratoryv1alpha1.LabList
	if err := a.Client.List(ctx, &labs, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	n := 0
	for i := range labs.Items {
		if labs.Items[i].DeletionTimestamp == nil && labs.Items[i].Spec.Internet.Enabled {
			n++
		}
	}
	return n, nil
}

func (a *GroupAutosizer) livePod(ctx context.Context, ns, component string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := a.Client.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{labelApp: component}); err != nil {
		return nil, err
	}
	var best *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best, nil
}

func containerOf(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func overheadOf(l corev1.ResourceList) grouppods.Overhead {
	return grouppods.Overhead{CPU: l.Cpu().MilliValue(), Memory: l.Memory().Value()}
}

func (a *GroupAutosizer) sizePod(ctx context.Context, lg *laboratoryv1alpha1.LabGroup, component string) (*laboratoryv1alpha1.GroupPodSizeStatus, error) {
	ns := lg.Status.Namespace
	pod, err := a.livePod(ctx, ns, component)
	if err != nil || pod == nil {
		return nil, err
	}
	c := containerOf(pod, component)
	if c == nil {
		return nil, nil
	}
	sizing, err := a.GroupPods.SizingOf(component)
	if err != nil {
		return nil, err
	}
	units, err := a.units(ctx, ns, component)
	if err != nil {
		return nil, err
	}
	request := overheadOf(c.Resources.Requests)
	cur := overheadOf(c.Resources.Limits)
	// What the pod has now is the floor of memory: its request (= its limit) is never lowered.
	floor := grouppods.Overhead{CPU: request.CPU, Memory: max(request.Memory, cur.Memory)}
	want := grouppods.Limit(sizing, floor, sizing.Max(), units, a.GroupPods.Autosize.Headroom())
	out := &laboratoryv1alpha1.GroupPodSizeStatus{Name: component, Units: int32(units), CPULimitMillicores: cur.CPU, MemoryLimitBytes: cur.Memory, State: podSizeApplied}

	guaranteed := pod.Status.QOSClass == corev1.PodQOSGuaranteed || pod.Status.QOSClass == "" && cur == request
	if guaranteed {
		out.State = podSizeFixed
		if want.CPU > cur.CPU || want.Memory > cur.Memory {
			out.State, out.Message = podSizeRecreate, "the pod was created Guaranteed (requests = limits) and cannot be resized in place: it grows when it is next recreated"
		}
		return out, nil
	}

	target := cur
	key := lg.Name + "/" + component
	switch {
	case want.CPU > cur.CPU:
		target.CPU = want.CPU
		delete(a.lowerSince, key)
	case want.CPU < cur.CPU:
		since, ok := a.lowerSince[key]
		if !ok {
			a.lowerSince[key] = a.now()
		} else if after := a.GroupPods.Autosize.ShrinkAfter; after > 0 && a.now().Sub(since) >= after {
			target.CPU = want.CPU
			delete(a.lowerSince, key)
		}
	default:
		delete(a.lowerSince, key)
	}
	if want.Memory > cur.Memory { // the memory of a live pod is never lowered
		target.Memory = want.Memory
	}
	if target == cur {
		a.describePending(pod, out)
		return out, nil
	}

	limitedBy := ""
	if target.Memory > cur.Memory {
		_, freeMemory, err := a.free(ctx, pod.Spec.NodeName)
		if err != nil {
			return nil, err
		}
		if target.Memory-cur.Memory > freeMemory {
			target.Memory = cur.Memory + max(freeMemory, 0)/grouppods.StepMemory*grouppods.StepMemory
			limitedBy = fmt.Sprintf("memory: the node %s has %d MiB free", pod.Spec.NodeName, max(freeMemory, 0)>>20)
		}
	}
	if target != cur {
		if err := a.applyResize(ctx, pod, component, target); err != nil {
			out.State, out.Message = podSizePending, "the resize was not accepted: "+err.Error()
			a.event(lg, corev1.EventTypeWarning, "PodResizeFailed", "%s pod: %s", component, out.Message)
			return out, nil
		}
		out.CPULimitMillicores, out.MemoryLimitBytes = target.CPU, target.Memory
		a.event(lg, corev1.EventTypeNormal, "PodResized", "%s pod %dm limit / %dMi -> %dm limit / %dMi for %d unit(s), no restart", component, cur.CPU, cur.Memory>>20, target.CPU, target.Memory>>20, units)
	}
	if limitedBy != "" {
		out.State, out.Message = podSizeLimited, "grown to what the node has room for; "+limitedBy
		a.event(lg, corev1.EventTypeWarning, "PodResizeLimited", "%s pod: %s", component, out.Message)
	}
	return out, nil
}

func (a *GroupAutosizer) describePending(pod *corev1.Pod, out *laboratoryv1alpha1.GroupPodSizeStatus) {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == "PodResizePending" && cond.Status == corev1.ConditionTrue {
			out.State, out.Message = podSizePending, "the kubelet has not applied the resize ("+cond.Reason+"): "+cond.Message
		}
	}
}

func (a *GroupAutosizer) event(lg *laboratoryv1alpha1.LabGroup, kind, reason, format string, args ...any) {
	if a.Recorder != nil {
		a.Recorder.Eventf(lg, kind, reason, format, args...)
	}
}

func (a *GroupAutosizer) applyResize(ctx context.Context, pod *corev1.Pod, container string, target grouppods.Overhead) error {
	if a.resize != nil {
		return a.resize(ctx, pod, container, target.CPU, target.Memory)
	}
	patch := map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
		"name": container,
		"resources": map[string]any{
			"requests": map[string]any{"memory": fmt.Sprintf("%d", target.Memory)},
			"limits": map[string]any{
				"cpu":    fmt.Sprintf("%dm", target.CPU),
				"memory": fmt.Sprintf("%d", target.Memory),
			},
		},
	}}}}
	raw, _ := json.Marshal(patch)
	return a.Client.SubResource("resize").Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, raw))
}

// free is what a node has left: its allocatable minus the requests of the pods on it that still hold their room.
func (a *GroupAutosizer) free(ctx context.Context, node string) (cpu, memory int64, err error) {
	if a.nodeFree != nil {
		return a.nodeFree(ctx, node)
	}
	var n corev1.Node
	if err := a.reader().Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return 0, 0, err
	}
	var pods corev1.PodList
	if err := a.reader().List(ctx, &pods, client.MatchingFields{"spec.nodeName": node}); err != nil {
		return 0, 0, err
	}
	cpu, memory = n.Status.Allocatable.Cpu().MilliValue(), n.Status.Allocatable.Memory().Value()
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for j := range p.Spec.Containers {
			r := p.Spec.Containers[j].Resources.Requests
			cpu, memory = cpu-r.Cpu().MilliValue(), memory-r.Memory().Value()
		}
	}
	return cpu, memory, nil
}

// publish writes the autosizing state into the group's status when it changed.
func (a *GroupAutosizer) publish(ctx context.Context, lg *laboratoryv1alpha1.LabGroup, results []laboratoryv1alpha1.GroupPodSizeStatus) error {
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	now := metav1.NewTime(a.now())
	prior := map[string]laboratoryv1alpha1.GroupPodSizeStatus{}
	for _, p := range lg.Status.PodSizing {
		prior[p.Name] = p
	}
	for i := range results {
		if p, ok := prior[results[i].Name]; ok && p.State == results[i].State && p.At != nil {
			results[i].At = p.At
		} else {
			results[i].At = &now
		}
	}
	if equality.Semantic.DeepEqual(lg.Status.PodSizing, results) {
		return nil
	}
	base := lg.DeepCopy()
	lg.Status.PodSizing = results
	if err := a.Client.Status().Patch(ctx, lg, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
