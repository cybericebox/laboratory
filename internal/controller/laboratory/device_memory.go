package laboratory

import (
	"context"
	stderrors "errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// ConditionMemoryAboveMaximum is the warning condition (True) of a Device or a Lab whose device already exists and is above the
// current limits.device.maxMemory: it keeps running (and is recreated) at its declared size, it is not refused.
const ConditionMemoryAboveMaximum = "MemoryAboveMaximum"

// deviceRan says the device has had a pod (or the scheduler sent it) under an earlier rule or maximum: a pod, a node, an incarnation, a
// dispatch or a start in its scheduling record, or a runtime inventory. Such a device is never refused for its memory: refusing it would
// stop a running lab, or leave its scheduling record holding a slot for nothing.
func deviceRan(d *laboratoryv1alpha1.Device) bool {
	if d == nil {
		return false
	}
	if d.Status.PodName != "" || d.Status.NodeName != "" || len(d.Status.RuntimeInventory) > 0 || d.Status.State != nil && d.Status.State.Incarnation > 0 {
		return true
	}
	sc := d.Status.Scheduling
	return sc != nil && (sc.DispatchedAt != nil || sc.StartedAt != nil || sc.State != "" && sc.State != laboratoryv1alpha1.PodQueued)
}

// refusedForMemory says a container device of this name and resources is refused for its memory: above limits.device.maxMemory and
// it never ran (d is its Device, nil when there is none yet). The device controller, the scheduler and the allocation count by it.
func refusedForMemory(d *laboratoryv1alpha1.Device, name string, res *laboratoryv1alpha1.DeviceResources, defaults DeviceDefaults) bool {
	return limits.CheckDeviceMemory(name, res, defaults) != nil && !deviceRan(d)
}

// memoryAboveMaximum is the refusal of a device whose memory is above limits.device.maxMemory (nil when it fits).
func (r *DeviceReconciler) memoryAboveMaximum(device *laboratoryv1alpha1.Device) error {
	if device.Spec.Type != laboratoryv1alpha1.DeviceTypeContainer {
		return nil
	}
	return limits.CheckDeviceMemory(device.Spec.Name, device.Spec.Resources, r.Defaults)
}

// isMemoryRefusal says err is the refusal of limits.CheckDeviceMemory.
func isMemoryRefusal(err error) bool {
	var refused *limits.MemoryAboveMaximumError
	return stderrors.As(err, &refused)
}

// refuseMemoryAboveMaximum applies owner decision C4 (refused, never clamped) to a device whose memory is above limits.device.maxMemory.
// A device that never ran gets no pod: Ready=False with the reason DeviceMemoryAboveMaximum and a message naming the requested and the
// maximum amounts. A device that already ran (exists: its workload or pod is there, or deviceRan) is left alone and recreated at its
// declared size; it gets the warning condition MemoryAboveMaximum instead. Either way one Warning event is recorded when the condition
// is set, and a device that fits again (the maximum was raised) loses both conditions. stop: the device is refused, or the status write
// failed (err).
func (r *DeviceReconciler) refuseMemoryAboveMaximum(ctx context.Context, device *laboratoryv1alpha1.Device, exists bool) (stop bool, err error) {
	refusal := r.memoryAboveMaximum(device)
	ran := exists || deviceRan(device)
	orig := device.DeepCopy()
	conds := &device.Status.Conditions
	if c := meta.FindStatusCondition(*conds, labstatus.ConditionReady); c != nil && c.Reason == limits.ReasonDeviceMemoryAboveMaximum && (refusal == nil || ran) {
		meta.RemoveStatusCondition(conds, labstatus.ConditionReady)
	}
	var msg string
	switch {
	case refusal == nil:
		meta.RemoveStatusCondition(conds, ConditionMemoryAboveMaximum)
	case ran:
		msg = refusal.Error() + ": it already ran, so it keeps running at that size and is not refused"
		meta.SetStatusCondition(conds, metav1.Condition{Type: ConditionMemoryAboveMaximum, Status: metav1.ConditionTrue, ObservedGeneration: device.Generation,
			Reason: limits.ReasonDeviceMemoryAboveMaximum, Message: msg})
	default:
		msg = refusal.Error() + ": the device is refused and does not start"
		meta.RemoveStatusCondition(conds, ConditionMemoryAboveMaximum)
		labstatus.SetReady(conds, device.Generation, false, limits.ReasonDeviceMemoryAboveMaximum, msg)
		device.Status.Ready = false
	}
	stop = refusal != nil && !ran
	if equality.Semantic.DeepEqual(orig.Status, device.Status) {
		return stop, nil
	}
	if msg != "" && r.Recorder != nil {
		r.Recorder.Event(device, corev1.EventTypeWarning, limits.ReasonDeviceMemoryAboveMaximum, msg)
	}
	if err := r.Status().Patch(ctx, device, client.MergeFrom(orig)); err != nil {
		return true, err
	}
	return stop, nil
}
