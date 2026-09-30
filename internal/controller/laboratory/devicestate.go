package laboratory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

const (
	// defaultExitSnapshotTimeout is how long a finished pod waits for the
	// node-agent's exit snapshot before the device is recreated from the last one.
	defaultExitSnapshotTimeout = 30 * time.Second
	// quickExit: a pod that ended sooner than this after starting counts as a crash loop step.
	quickExit = 30 * time.Second
	// maxRestartDelay caps the back-off between recreations of a crashing device.
	maxRestartDelay = 2 * time.Minute
)

// SnapshotRegistry is the part of the snapshot registry the controller needs:
// dropping a device's snapshots on reset.
type SnapshotRegistry interface {
	DeleteRepo(ctx context.Context, repo string) error
}

// deviceStateEnabled reports whether the device runs as a snapshot-backed Pod.
func deviceStateEnabled(d *laboratoryv1alpha1.Device) bool {
	return d.Spec.State != nil && d.Spec.State.Enabled
}

// stableDeviceMAC derives a locally administered unicast MAC from the device
// identity, so every incarnation of a device has the same hardware address.
func stableDeviceMAC(namespace, device, iface string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + device + "/" + iface))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

func podName(device *laboratoryv1alpha1.Device, incarnation int32) string {
	return fmt.Sprintf("%s-%d", device.Name, incarnation)
}

func (r *DeviceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *DeviceReconciler) exitTimeout() time.Duration {
	if r.ExitSnapshotTimeout > 0 {
		return r.ExitSnapshotTimeout
	}
	return defaultExitSnapshotTimeout
}

// restartDelay is the back-off before recreating a device whose pods keep
// ending right after they start.
func restartDelay(streak int32) time.Duration {
	if streak <= 0 {
		return 0
	}
	if streak > 10 {
		return maxRestartDelay
	}
	d := 2 * time.Second << (streak - 1)
	if d > maxRestartDelay {
		return maxRestartDelay
	}
	return d
}

// statePatch is a JSON merge patch of status.state. A nil map value removes the field.
type statePatch map[string]any

func (r *DeviceReconciler) patchState(ctx context.Context, device *laboratoryv1alpha1.Device, p statePatch) error {
	raw, err := json.Marshal(map[string]any{"status": map[string]any{"state": p}})
	if err != nil {
		return err
	}
	if err := r.Status().Patch(ctx, device, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return err
	}
	if device.Status.State == nil {
		device.Status.State = &laboratoryv1alpha1.DeviceStateStatus{}
	}
	return nil
}

func isTerminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// devicePods lists the pods owned by the device, sorted by name.
func (r *DeviceReconciler) devicePods(ctx context.Context, device *laboratoryv1alpha1.Device) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list, client.InNamespace(device.Namespace), client.MatchingLabels{
		names.LabelLab:    device.Spec.LabRef,
		names.LabelDevice: device.Spec.Name,
	}); err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], device) {
			pods = append(pods, list.Items[i])
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

// reconcilePod runs a snapshot-backed device as a bare Pod (restartPolicy
// Never) that this controller owns and recreates. A pod that ends (crash, OOM,
// eviction, node loss) is replaced from the latest snapshot of the device's
// writable layer, once the node-agent finished the exit snapshot or the wait for
// it timed out. The pod name carries an incarnation number, so a replacement
// never shares a name (and an OVS port) with the pod it replaces.
func (r *DeviceReconciler) reconcilePod(ctx context.Context, device *laboratoryv1alpha1.Device) (ctrl.Result, error) {
	suspended, err := r.labGroupSuspended(ctx, device.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePodDisruptionBudget(ctx, device); err != nil {
		return ctrl.Result{}, err
	}
	if device.Status.State == nil {
		if err := r.patchState(ctx, device, statePatch{"epoch": 0}); err != nil {
			return ctrl.Result{}, err
		}
	}
	st := device.Status.State
	spec := device.Spec.State

	pods, err := r.devicePods(ctx, device)
	if err != nil {
		return ctrl.Result{}, err
	}

	if spec.ResetToken != st.ResetToken {
		return r.resetDevice(ctx, device, pods)
	}

	name := podName(device, st.Incarnation)
	var cur *corev1.Pod
	for i := range pods {
		if pods[i].Name == name {
			cur = &pods[i]
			continue
		}
		// A pod of an older incarnation that is still around: drop it.
		if pods[i].DeletionTimestamp == nil {
			if err := r.Delete(ctx, &pods[i]); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if cur != nil {
		return r.reconcileCurrentPod(ctx, device, cur, suspended)
	}

	// No pod of the current incarnation.
	if err := r.publishPlacement(ctx, device, nil); err != nil {
		return ctrl.Result{}, err
	}
	if suspended {
		return ctrl.Result{}, nil
	}
	if len(pods) > 0 {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil // an old pod is still going away
	}
	if wait := r.recreateWait(device); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	if err := r.createDevicePod(ctx, device); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// recreateWait is how long to hold off creating the next pod: until the exit
// snapshot of the previous one is done (or timed out) and the crash back-off passed.
func (r *DeviceReconciler) recreateWait(device *laboratoryv1alpha1.Device) time.Duration {
	st := device.Status.State
	if st.Incarnation == 0 || st.StoppedAt == nil {
		return 0
	}
	now := r.now()
	var wait time.Duration
	if st.ExitSnapshotPod != podName(device, st.Incarnation) {
		if left := r.exitTimeout() - now.Sub(st.StoppedAt.Time); left > 0 {
			wait = left
		}
	}
	if left := restartDelay(st.CrashStreak) - now.Sub(st.StoppedAt.Time); left > wait {
		wait = left
	}
	return wait
}

func (r *DeviceReconciler) reconcileCurrentPod(ctx context.Context, device *laboratoryv1alpha1.Device, cur *corev1.Pod, suspended bool) (ctrl.Result, error) {
	st := device.Status.State
	spec := device.Spec.State
	now := r.now()

	if cur.DeletionTimestamp != nil {
		if err := r.markStopped(ctx, device, now); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.publishPlacement(ctx, device, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if isTerminal(cur) {
		return r.reconcileEndedPod(ctx, device, cur)
	}

	// A running or starting pod that no longer matches the wanted mode is
	// recycled: its exit snapshot is taken, then it is recreated.
	epoch := fmt.Sprint(st.Epoch)
	if suspended || cur.Annotations[names.AnnotationStateEpoch] != epoch ||
		(cur.Annotations[names.AnnotationStateRescue] == "true") != spec.Rescue {
		if err := r.markStopped(ctx, device, now); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Delete(ctx, cur); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		if err := r.publishPlacement(ctx, device, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if err := r.publishPlacement(ctx, device, cur); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reportSnapshotPull(ctx, device, cur); err != nil {
		return ctrl.Result{}, err
	}
	if !podReady(cur) || cur.Status.PodIP == "" {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// pullWarning prefixes the warning the controller sets when a snapshot image cannot be pulled.
const pullWarning = "snapshot image unavailable"

// reportSnapshotPull surfaces a pod stuck pulling its snapshot image as a device
// warning (there is no silent fallback to the base image, which would lose the
// state), and clears the warning once the pod runs.
func (r *DeviceReconciler) reportSnapshotPull(ctx context.Context, device *laboratoryv1alpha1.Device, pod *corev1.Pod) error {
	st := device.Status.State
	failing := ""
	if st.Image != "" && len(pod.Spec.Containers) > 0 && pod.Spec.Containers[0].Image == st.Image {
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
				failing = w.Reason
			}
		}
	}
	switch {
	case failing != "" && !strings.HasPrefix(st.Warning, pullWarning):
		return r.patchState(ctx, device, statePatch{"warning": pullWarning + " (" + failing + "): the registry cannot be reached; reset the device to start from the base image"})
	case failing == "" && strings.HasPrefix(st.Warning, pullWarning) && pod.Status.Phase == corev1.PodRunning:
		return r.patchState(ctx, device, statePatch{"warning": nil})
	}
	return nil
}

// reconcileEndedPod handles a pod whose container finished: wait for the exit
// snapshot, count crash-loop steps, delete the pod; the next reconcile creates
// its replacement.
func (r *DeviceReconciler) reconcileEndedPod(ctx context.Context, device *laboratoryv1alpha1.Device, cur *corev1.Pod) (ctrl.Result, error) {
	st := device.Status.State
	now := r.now()
	started, finished := containerTimes(cur)
	if finished.IsZero() {
		finished = now
	}
	if st.StoppedAt == nil {
		if err := r.patchState(ctx, device, statePatch{"stoppedAt": finished.UTC().Format(time.RFC3339)}); err != nil {
			return ctrl.Result{}, err
		}
		st = device.Status.State
		st.StoppedAt = &metav1.Time{Time: finished}
	}
	if err := r.publishPlacement(ctx, device, nil); err != nil {
		return ctrl.Result{}, err
	}
	if st.ExitSnapshotPod != cur.Name {
		if left := r.exitTimeout() - now.Sub(st.StoppedAt.Time); left > 0 {
			return ctrl.Result{RequeueAfter: left}, nil
		}
	}
	streak := int32(0)
	if !started.IsZero() && finished.Sub(started) < quickExit {
		streak = st.CrashStreak + 1
	}
	if streak != st.CrashStreak {
		if err := r.patchState(ctx, device, statePatch{"crashStreak": streak}); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.Delete(ctx, cur); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// containerTimes returns when the device container started and finished.
func containerTimes(p *corev1.Pod) (started, finished time.Time) {
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return t.StartedAt.Time, t.FinishedAt.Time
		}
	}
	return time.Time{}, time.Time{}
}

// markStopped records when the controller first saw the current pod end or ordered it to.
func (r *DeviceReconciler) markStopped(ctx context.Context, device *laboratoryv1alpha1.Device, at time.Time) error {
	if device.Status.State.StoppedAt != nil {
		return nil
	}
	if err := r.patchState(ctx, device, statePatch{"stoppedAt": at.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	device.Status.State.StoppedAt = &metav1.Time{Time: at}
	return nil
}

// publishPlacement mirrors the live pod into the device status (nil: none).
func (r *DeviceReconciler) publishPlacement(ctx context.Context, device *laboratoryv1alpha1.Device, pod *corev1.Pod) error {
	var node, ip, name string
	ready, rescue := false, false
	if pod != nil {
		node, name = pod.Spec.NodeName, pod.Name
		if pod.Status.Phase == corev1.PodRunning {
			ip = pod.Status.PodIP
		}
		ready = podReady(pod)
		rescue = pod.Annotations[names.AnnotationStateRescue] == "true"
	}
	st := device.Status.State
	if node == device.Status.NodeName && ip == device.Status.PodIP && name == device.Status.PodName &&
		ready == device.Status.Ready && rescue == st.Rescue {
		return nil
	}
	orig := device.DeepCopy()
	device.Status.NodeName, device.Status.PodIP, device.Status.PodName, device.Status.Ready = node, ip, name, ready
	device.Status.State.Rescue = rescue
	return r.Status().Patch(ctx, device, client.MergeFrom(orig))
}

// createDevicePod starts the next incarnation of the device.
func (r *DeviceReconciler) createDevicePod(ctx context.Context, device *laboratoryv1alpha1.Device) error {
	st := device.Status.State
	spec := device.Spec.State
	next := st.Incarnation + 1
	image := device.Spec.Image
	restored := false
	if st.Image != "" {
		image, restored = st.Image, true
	}
	patch := statePatch{"incarnation": next, "stoppedAt": nil}
	if restored {
		patch["restoredAt"] = r.now().UTC().Format(time.RFC3339)
	}
	if err := r.patchState(ctx, device, patch); err != nil {
		return err
	}
	device.Status.State.Incarnation = next
	device.Status.State.StoppedAt = nil

	labels, _, annotations, podSpec := r.workloadTemplate(device, true)
	annotations[names.AnnotationStateEpoch] = fmt.Sprint(st.Epoch)
	annotations[names.AnnotationStateDevice] = device.Name
	annotations[names.AnnotationStateIncarnation] = fmt.Sprint(next)
	if spec.Rescue {
		annotations[names.AnnotationStateRescue] = "true"
	}
	podSpec.RestartPolicy = corev1.RestartPolicyNever
	c := &podSpec.Containers[0]
	c.Image = image
	if spec.Rescue {
		// Start the snapshot with a shell instead of the entrypoint, so a
		// configuration that makes the service crash can be repaired in place.
		c.Command = []string{"/bin/sh", "-c", "trap : TERM INT; while :; do sleep 3600 & wait $!; done"}
		c.Args = nil
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName(device, next),
			Namespace:   device.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: podSpec,
	}
	if err := controllerutil.SetControllerReference(device, pod, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// resetDevice discards the snapshots of a device and starts it from its base
// image: stop the pods, wait for their exit snapshots to end (so none lands
// after the delete), delete the repository, then bump the epoch.
func (r *DeviceReconciler) resetDevice(ctx context.Context, device *laboratoryv1alpha1.Device, pods []corev1.Pod) (ctrl.Result, error) {
	st := device.Status.State
	now := r.now()
	if len(pods) > 0 {
		if err := r.markStopped(ctx, device, now); err != nil {
			return ctrl.Result{}, err
		}
		for i := range pods {
			if pods[i].DeletionTimestamp == nil {
				if err := r.Delete(ctx, &pods[i]); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if st.Incarnation > 0 && st.StoppedAt != nil && st.ExitSnapshotPod != podName(device, st.Incarnation) {
		if left := r.exitTimeout() - now.Sub(st.StoppedAt.Time); left > 0 {
			return ctrl.Result{RequeueAfter: left}, nil
		}
	}
	if r.Registry != nil {
		repo := snapshot.Repo(device.Namespace, device.Spec.LabRef, device.Spec.Name)
		if err := r.Registry.DeleteRepo(ctx, repo); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete snapshots %s: %w", repo, err)
		}
	}
	if err := r.patchState(ctx, device, statePatch{
		"epoch":           st.Epoch + 1,
		"resetToken":      device.Spec.State.ResetToken,
		"image":           nil,
		"snapshotAt":      nil,
		"sizeBytes":       nil,
		"layers":          nil,
		"warning":         nil,
		"restoredAt":      nil,
		"stoppedAt":       nil,
		"crashStreak":     nil,
		"exitSnapshotPod": nil,
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}
