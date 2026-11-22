package laboratory

import (
	"context"
	stderrors "errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

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

// refuseMemoryAboveMaximum fails closed for a device whose memory is above limits.device.maxMemory (owner decision C4: refused, never
// clamped): no pod is made, the device gets Ready=False with the reason DeviceMemoryAboveMaximum and a message naming the requested and
// the maximum amounts, and a Warning event (once, when the condition is set). It reports stop: the device is refused, or the status
// write failed (err). A device that fits (the maximum was raised) loses the condition and starts as usual. hasPod: a pod of the device
// exists already (made before this rule); it is left alone and nothing is refused.
func (r *DeviceReconciler) refuseMemoryAboveMaximum(ctx context.Context, device *laboratoryv1alpha1.Device, hasPod bool) (stop bool, err error) {
	if hasPod {
		return false, nil
	}
	refusal := r.memoryAboveMaximum(device)
	cond := meta.FindStatusCondition(device.Status.Conditions, labstatus.ConditionReady)
	if refusal == nil {
		if cond == nil || cond.Reason != limits.ReasonDeviceMemoryAboveMaximum {
			return false, nil
		}
		orig := device.DeepCopy()
		meta.RemoveStatusCondition(&device.Status.Conditions, labstatus.ConditionReady)
		err := r.Status().Patch(ctx, device, client.MergeFrom(orig))
		return err != nil, err
	}
	msg := refusal.Error()
	if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == limits.ReasonDeviceMemoryAboveMaximum && cond.Message == msg &&
		cond.ObservedGeneration == device.Generation && !device.Status.Ready {
		return true, nil
	}
	if r.Recorder != nil {
		r.Recorder.Event(device, corev1.EventTypeWarning, limits.ReasonDeviceMemoryAboveMaximum, msg)
	}
	orig := device.DeepCopy()
	labstatus.SetReady(&device.Status.Conditions, device.Generation, false, limits.ReasonDeviceMemoryAboveMaximum, msg)
	device.Status.Ready = false
	return true, r.Status().Patch(ctx, device, client.MergeFrom(orig))
}
