package laboratory

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The scheduler decides when a device's pod may be created. The device
// reconciler only keeps the pod's scheduling record initialised: Queued for a
// device that has no workload yet (it waits for the scheduler to dispatch it),
// Started for one that already runs (a device that predates the scheduler, or a
// workload the scheduler never had to start). It never moves a record on.

// queuedByScheduler reports whether the scheduler holds the device's pod back.
func (r *DeviceReconciler) queuedByScheduler(device *laboratoryv1alpha1.Device) bool {
	return r.Scheduled && device.Status.Scheduling != nil && device.Status.Scheduling.State == laboratoryv1alpha1.PodQueued
}

// initScheduling gives a device with no record its first one.
func (r *DeviceReconciler) initScheduling(ctx context.Context, device *laboratoryv1alpha1.Device, workloadExists bool) error {
	if !r.Scheduled || device.Status.Scheduling != nil {
		return nil
	}
	now := metav1.NewTime(r.now())
	ps := &laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodQueued, QueuedAt: &now}
	if workloadExists {
		ps = &laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodStarted, StartedAt: &now}
	}
	orig := device.DeepCopy()
	device.Status.Scheduling = ps
	return r.Status().Patch(ctx, device, client.MergeFrom(orig))
}

// mayCreateWorkload reports whether the device's pod may be created now. A device
// with no record is queued first; with a record it may go once it is not Queued.
func (r *DeviceReconciler) mayCreateWorkload(ctx context.Context, device *laboratoryv1alpha1.Device) (bool, error) {
	if stopped, err := r.deviceStopped(ctx, device); stopped || err != nil {
		return false, err
	}
	if !r.Scheduled {
		return true, nil
	}
	// A cached Started record from before stop is not admission for this Start.
	// Read the scheduling owner directly before resuming a retained workload.
	var current laboratoryv1alpha1.Device
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(device), &current); err != nil {
		return false, err
	}
	if current.UID != device.UID {
		return false, nil
	}
	if err := r.initScheduling(ctx, &current, false); err != nil {
		return false, err
	}
	device.Status.Scheduling = current.Status.Scheduling.DeepCopy()
	var parent laboratoryv1alpha1.Lab
	if device.Spec.LabRef != "" {
		if err := r.reader().Get(ctx, client.ObjectKey{Name: device.Spec.LabRef, Namespace: device.Namespace}, &parent); err != nil {
			return false, err
		}
	}
	if parent.Spec.Lifecycle != nil && parent.Spec.Lifecycle.DesiredState == "Running" {
		return labStartPrepared(&parent) && current.Status.Scheduling.DispatchedAt != nil && !r.queuedByScheduler(&current), nil
	}
	return !r.queuedByScheduler(&current), nil
}
