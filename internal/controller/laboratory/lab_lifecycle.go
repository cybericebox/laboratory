package laboratory

import (
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

func (r *LabReconciler) lifecycleReader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// ValidateRequiredSnapshot is the preparation/acceptance seam. Task4 must call it
// before writing Required intent. Capability stays off until the native proof.
func (r *LabReconciler) ValidateRequiredSnapshot(ctx context.Context, l *lab.Lab) error {
	hasWritableDevice := false
	for _, d := range l.Spec.Devices {
		if d.Type == lab.DeviceTypeHub || d.Type == lab.DeviceTypeUnmanagedSwitch {
			continue
		}
		hasWritableDevice = true
		if d.Type != lab.DeviceTypeContainer || d.Persistence == nil || !d.Persistence.Enabled {
			return fmt.Errorf("device %s does not support required persistence", d.Name)
		}
	}
	if hasWritableDevice && !r.RequiredSnapshotAvailable {
		return fmt.Errorf("required snapshot freezer/registry capability is unavailable")
	}
	var ds lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &ds, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	materialized := map[string]bool{}
	for i := range ds.Items {
		d := &ds.Items[i]
		if !ownedLabDevice(l, d) {
			continue
		}
		if d.Spec.Type == lab.DeviceTypeHub || d.Spec.Type == lab.DeviceTypeUnmanagedSwitch {
			continue
		}
		materialized[d.Spec.Name] = true
		ready := false
		for _, report := range d.Status.RuntimeReports {
			id := report.Identity
			if report.Error == "" && report.ObservedAt != nil && time.Since(report.ObservedAt.Time) >= 0 && time.Since(report.ObservedAt.Time) <= 60*time.Second && id.OwnerUID == string(l.UID) && id.NodeName == d.Status.NodeName && id.PodUID != "" && id.NodeBootID != "" && len(id.ContainerIDs) > 0 && len(id.CgroupPaths) > 0 && d.Status.State != nil && id.Epoch == d.Status.State.Epoch && id.Incarnation == d.Status.State.Incarnation {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("device %s has no fresh native checkpoint handshake", d.Name)
		}
		if d.Spec.Type != lab.DeviceTypeContainer || !deviceStateEnabled(d) {
			return fmt.Errorf("device %s has no supported persistence", d.Name)
		}
	}
	var ten lab.Tenant
	tenantName := names.TenantOf(l.Labels)
	var tenantPolicy *lab.Tenant
	for _, template := range l.Spec.Devices {
		if template.Type != lab.DeviceTypeContainer || materialized[template.Name] {
			continue
		}
		if tenantPolicy == nil {
			err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: tenantName}, &ten)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil {
				tenantPolicy = &ten
			}
		}
		if r.deviceStateSpec(tenantPolicy, template) == nil {
			return fmt.Errorf("device %s cannot prepare persistence under current platform/tenant policy", template.Name)
		}
	}
	return nil
}
func ownedLabDevice(l *lab.Lab, d *lab.Device) bool {
	if d.Spec.LabRef != l.Name {
		return false
	}
	for _, o := range d.OwnerReferences {
		if o.Kind == "Lab" && o.UID == l.UID && o.Name == l.Name {
			return true
		}
	}
	return false
}
func ownedDevicePod(d *lab.Device, p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "Device" && o.UID == d.UID && o.Name == d.Name {
			return true
		}
	}
	return false
}
func (r *DeviceReconciler) deviceStopped(ctx context.Context, d *lab.Device) (bool, error) {
	if d.Spec.LabRef == "" {
		return false, nil
	}
	var l lab.Lab
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: d.Spec.LabRef}, &l); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return true, err
	}
	for _, owner := range d.OwnerReferences {
		if owner.Kind == "Lab" && owner.Name == l.Name && owner.UID != l.UID {
			return true, nil
		}
	}
	return l.Spec.Lifecycle.IsStopped() || l.Spec.Lifecycle != nil && l.Spec.Lifecycle.Terminal || !labStartPrepared(&l), nil
}

// Lifecycle is checked before every ordinary materialization path. This owner
// requests stop but never infers native death or release from an absent API Pod.
func (r *LabReconciler) reconcileLifecycle(ctx context.Context, l *lab.Lab) (bool, ctrl.Result, error) {
	var current lab.Lab
	if err := r.lifecycleReader().Get(ctx, client.ObjectKeyFromObject(l), &current); err != nil {
		return true, ctrl.Result{}, err
	}
	*l = current
	intent := l.Spec.Lifecycle
	if intent == nil {
		return false, ctrl.Result{}, nil
	}
	if !intent.IsStopped() {
		return r.reconcileLifecycleStart(ctx, l)
	}
	// Task5 is the only native release producer. A matching existing native
	// observation is consumed without inventing one or repeating capture/deletion.
	if exactStoppedRelease(l) {
		next := l.Status.Lifecycle.DeepCopy()
		ok, at, boot, err := r.currentAccessFence(ctx, l)
		if err != nil {
			return true, ctrl.Result{}, err
		}
		next.AccessFenced = ok
		next.AccessFencedAt = at
		next.AccessFenceVPNBootID = boot
		return true, ctrl.Result{RequeueAfter: 30 * time.Second}, r.patchLifecycle(ctx, l, next)
	}
	next := &lab.LabLifecycleStatus{ObservedState: "Stopping", OperationID: intent.OperationID, Revision: intent.Revision, LabUID: string(l.UID), ObservedGeneration: l.Generation, RequestedAt: ptrTime(metav1.Now())}
	if old := l.Status.Lifecycle; old != nil && old.OperationID == intent.OperationID && old.Revision == intent.Revision && old.LabUID == string(l.UID) && old.ObservedGeneration == l.Generation {
		next.RequestedAt = old.RequestedAt
		next.SnapshotComplete = old.SnapshotComplete
	}
	finish := func(state, reason string, err error) (bool, ctrl.Result, error) {
		if next.SnapshotComplete && (state == "StopFailed" || state == "Snapshotting") {
			state = "Unknown"
			reason = "CommittedStopBlocked: " + reason
		}
		next.ObservedState = state
		if state == "StopFailed" {
			next.SnapshotComplete = false
			if cancelErr := r.cancelLifecycleCaptures(ctx, l); cancelErr != nil {
				return true, ctrl.Result{}, cancelErr
			}
		}
		next.Reason = reason
		if err != nil {
			next.Error = err.Error()
		}
		return true, ctrl.Result{RequeueAfter: 2 * time.Second}, r.patchLifecycle(ctx, l, next)
	}
	if intent.SnapshotMode == "Required" {
		if err := r.ValidateRequiredSnapshot(ctx, l); err != nil {
			return finish("StopFailed", "PreparationFailed", err)
		}
	}
	if intent.SnapshotMode != "Skip" && intent.SnapshotMode != "Required" {
		return finish("StopFailed", "InvalidSnapshotPolicy", fmt.Errorf("explicit snapshot mode required"))
	}
	fenced, at, boot, err := r.currentAccessFence(ctx, l)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	next.AccessFenced = fenced
	next.AccessFencedAt = at
	next.AccessFenceVPNBootID = boot
	if old := l.Status.Lifecycle; old != nil && old.ObservedState == "StopFailed" && old.OperationID == intent.OperationID && old.Revision == intent.Revision && old.ObservedGeneration == l.Generation {
		return finish("StopFailed", old.Reason, fmt.Errorf("%s", old.Error))
	}
	if !fenced {
		return finish("Stopping", "WaitingForAccessFence", nil)
	}
	var ds lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &ds, client.InNamespace(l.Namespace)); err != nil {
		return true, ctrl.Result{}, err
	}
	type runtime struct {
		d    *lab.Device
		pods []corev1.Pod
	}
	var targets []runtime
	for i := range ds.Items {
		d := &ds.Items[i]
		if !ownedLabDevice(l, d) {
			continue
		}
		var ps corev1.PodList
		if err := r.lifecycleReader().List(ctx, &ps, client.InNamespace(l.Namespace)); err != nil {
			return true, ctrl.Result{}, err
		}
		var pods []corev1.Pod
		for _, p := range ps.Items {
			owned, e := r.ownedRuntimeDevicePod(ctx, d, &p)
			if e != nil {
				return true, ctrl.Result{}, e
			}
			if owned {
				pods = append(pods, p)
			}
		}
		targets = append(targets, runtime{d, pods})
	}
	// Capture the complete native identity in controller-owned durable inventory
	// before any scale/delete. Unknown and force-deleted Pods keep prior holdings.
	for _, target := range targets {
		d := target.d
		if d.Spec.Type != lab.DeviceTypeContainer {
			continue
		}
		base := d.DeepCopy()
		for _, p := range target.pods {
			found := false
			for _, report := range d.Status.RuntimeReports {
				id := report.Identity
				if id.OwnerUID == string(l.UID) && id.OperationID == intent.OperationID && id.Revision == intent.Revision && id.PodUID == string(p.UID) && id.NodeName == p.Spec.NodeName && id.NodeBootID != "" && len(id.ContainerIDs) > 0 && len(id.CgroupPaths) > 0 && len(id.PortKeys) > 0 {
					exists := false
					for _, old := range d.Status.RuntimeInventory {
						exists = exists || reflect.DeepEqual(old, id)
					}
					if !exists {
						d.Status.RuntimeInventory = append(d.Status.RuntimeInventory, id)
					}
					found = true
					break
				}
			}
			if !found {
				return finish("Unknown", "WaitingForNativeInventory", nil)
			}
		}
		if len(target.pods) == 0 && len(d.Status.RuntimeInventory) == 0 {
			return finish("Unknown", "MissingNativeInventory", nil)
		}
		if !reflect.DeepEqual(base.Status.RuntimeInventory, d.Status.RuntimeInventory) {
			if err := r.Status().Patch(ctx, d, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return true, ctrl.Result{}, err
			}
		}
	}
	// First prepare every request; then validate every guard; no deletion can occur
	// in either pass. Capture-time RV is audit data only.
	pending := false
	if intent.SnapshotMode == "Required" {
		for _, target := range targets {
			d := target.d
			if d.Spec.Type != lab.DeviceTypeContainer {
				continue
			}
			for i := range target.pods {
				p := &target.pods[i]
				if p.DeletionTimestamp != nil && next.SnapshotComplete {
					continue
				}
				if p.DeletionTimestamp != nil {
					return finish("Unknown", "CapturePodDeleting", nil)
				}
				if d.Status.State == nil {
					return finish("Unknown", "MissingDeviceState", nil)
				}
				req := d.Spec.State.CaptureRequest
				if req == nil || req.OperationID != intent.OperationID || req.LifecycleRevision != intent.Revision || req.PodUID != string(p.UID) || req.Epoch != d.Status.State.Epoch || req.Incarnation != d.Status.State.Incarnation {
					base := d.DeepCopy()
					d.Spec.State.CaptureRequest = &lab.DeviceCaptureRequest{OperationID: intent.OperationID, LifecycleRevision: intent.Revision, PodUID: string(p.UID), PodResourceVersion: p.ResourceVersion, Epoch: d.Status.State.Epoch, Incarnation: d.Status.State.Incarnation, DeadlineSeconds: 300}
					if err := r.Patch(ctx, d, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
						return true, ctrl.Result{}, err
					}
					pending = true
					continue
				}
				result := d.Status.State.Capture
				if result != nil && result.OperationID == intent.OperationID && result.LifecycleRevision == intent.Revision && result.PodUID == string(p.UID) && result.Result == "Failed" {
					return finish("StopFailed", "RequiredCaptureFailed", fmt.Errorf("%s: %s", d.Name, result.Error))
				}
				if !devicestate.CaptureGuardMatches(p, req, result) {
					pending = true
				}
			}
			if len(target.pods) == 0 && (d.Status.PodName != "" || d.Status.State != nil && d.Status.State.Incarnation > 0) && !next.SnapshotComplete {
				return finish("Unknown", "MissingRuntimeObservation", nil)
			}
		}
		if pending {
			return finish("Snapshotting", "WaitingForRequiredCapture", nil)
		}
		// Captures are collectively successful and held. Ask every exact boot to
		// fsync its commit journal and acknowledge in both Pod guard and Device.
		// No first delete is authorized by capture success alone.
		commitsPending := false
		for _, target := range targets {
			d := target.d
			if d.Spec.Type != lab.DeviceTypeContainer {
				continue
			}
			for i := range target.pods {
				p := &target.pods[i]
				if p.DeletionTimestamp != nil && next.SnapshotComplete {
					continue
				}
				if d.Status.State == nil || d.Status.State.Capture == nil {
					return finish("Unknown", "MissingCommitCapture", nil)
				}
				result := d.Status.State.Capture
				request := d.Spec.State.CaptureRequest
				if request.CommitNodeAgentEpoch != result.NodeAgentEpoch {
					base := d.DeepCopy()
					d.Spec.State.CaptureRequest.CommitNodeAgentEpoch = result.NodeAgentEpoch
					if err := r.Patch(ctx, d, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
						return true, ctrl.Result{}, err
					}
					commitsPending = true
					continue
				}
				if !devicestate.CaptureCommittedGuardMatches(p, request, result) {
					commitsPending = true
				}
			}
		}
		if commitsPending {
			return finish("Snapshotting", "WaitingForDurableCaptureCommit", nil)
		}
		next.SnapshotComplete = true
		// Persist the barrier before any irreversible delete request; retries retain
		// the exact operation only. Native release is still a separate observation.
		if err := r.patchLifecycle(ctx, l, next); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	for _, target := range targets {
		d := target.d
		var live lab.Lab
		if err := r.lifecycleReader().Get(ctx, client.ObjectKeyFromObject(l), &live); err != nil {
			return true, ctrl.Result{}, err
		}
		if live.UID != l.UID || !reflect.DeepEqual(live.Spec.Lifecycle, intent) {
			return true, ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if ok, _, _, err := r.currentAccessFence(ctx, &live); err != nil || !ok {
			return true, ctrl.Result{RequeueAfter: time.Second}, err
		}
		var dep appsv1.Deployment
		if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: workloadName(d), Namespace: d.Namespace}, &dep); err == nil {
			owned := false
			for _, o := range dep.OwnerReferences {
				owned = owned || o.Kind == "Device" && o.UID == d.UID
			}
			if owned && (dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0) {
				base := dep.DeepCopy()
				dep.Spec.Replicas = ptrInt32(0)
				if err := r.Patch(ctx, &dep, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return true, ctrl.Result{}, err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, err
		}
		if !deviceStateEnabled(d) {
			continue
		}
		for _, old := range target.pods {
			var fresh lab.Device
			if err := r.lifecycleReader().Get(ctx, client.ObjectKeyFromObject(d), &fresh); err != nil {
				return true, ctrl.Result{}, err
			}
			if fresh.UID != d.UID {
				return finish("Unknown", "DeviceIdentityChanged", nil)
			}
			var pod corev1.Pod
			if err := r.lifecycleReader().Get(ctx, client.ObjectKeyFromObject(&old), &pod); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return true, ctrl.Result{}, err
			}
			if !ownedDevicePod(&fresh, &pod) {
				return finish("Unknown", "PodIdentityChanged", nil)
			}
			if pod.DeletionTimestamp != nil {
				continue
			}
			pre := &metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}
			if intent.SnapshotMode == "Required" {
				if fresh.Status.State == nil || fresh.Spec.State == nil {
					return finish("Unknown", "CaptureMissing", nil)
				}
				var err error
				pre, err = devicestate.CaptureCommittedDeletePreconditions(&pod, fresh.Spec.State.CaptureRequest, fresh.Status.State.Capture)
				if err != nil {
					return finish("Snapshotting", "CaptureGuardChanged", nil)
				}
				req := fresh.Spec.State.CaptureRequest
				if req.OperationID != intent.OperationID || req.LifecycleRevision != intent.Revision || req.Epoch != fresh.Status.State.Epoch || req.Incarnation != fresh.Status.State.Incarnation {
					return finish("Snapshotting", "CaptureIdentityChanged", nil)
				}
			}
			if err := r.Delete(ctx, &pod, client.Preconditions(*pre)); err != nil {
				if apierrors.IsConflict(err) {
					return true, ctrl.Result{RequeueAfter: time.Second}, nil
				}
				if !apierrors.IsNotFound(err) {
					return true, ctrl.Result{}, err
				}
			}
		}
	}
	// Task5 is the native observation owner. No API-only path can fabricate its
	// Released acknowledgement, including queued or switch-only topology.
	var rows []lab.OwnedRuntimeIdentity
	var reports []lab.OwnedRuntimeReport
	var quota int64
	for _, target := range targets {
		rows = append(rows, target.d.Status.RuntimeInventory...)
		reports = append(reports, target.d.Status.RuntimeReports...)
		if target.d.Status.State != nil {
			quota += target.d.Status.State.SizeBytes
		}
	}
	allocation := aggregateRuntime(rows, reports, string(l.UID), intent.OperationID, intent.Revision)
	allocation.SnapshotQuotaBytes = quota
	if quota > 0 {
		allocation.StorageState = "Retained"
	}
	base := l.DeepCopy()
	l.Status.Resources = allocation
	if err := r.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return true, ctrl.Result{}, err
	}
	if allocation.RuntimeState == "Released" {
		next.StoppedAt = ptrTime(metav1.Now())
		return finish("Stopped", "NativeRuntimeAndFabricReleased", nil)
	}
	return finish("Unknown", "WaitingForNativeRuntimeAndFabricObservation", nil)
}
func ptrTime(t metav1.Time) *metav1.Time { return &t }
func (r *LabReconciler) patchLifecycle(ctx context.Context, l *lab.Lab, next *lab.LabLifecycleStatus) error {
	if reflect.DeepEqual(l.Status.Lifecycle, next) {
		return nil
	}
	base := l.DeepCopy()
	l.Status.Lifecycle = next
	return r.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
func (r *LabReconciler) reconcileLifecycleStart(ctx context.Context, l *lab.Lab) (bool, ctrl.Result, error) {
	old := l.Status.Lifecycle
	if old != nil && old.OperationID == l.Spec.Lifecycle.OperationID && old.Revision == l.Spec.Lifecycle.Revision && old.LabUID == string(l.UID) {
		return false, ctrl.Result{}, nil
	}
	if l.Spec.Lifecycle.Terminal {
		return true, ctrl.Result{}, fmt.Errorf("terminal laboratory cannot start")
	}
	var ds lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &ds, client.InNamespace(l.Namespace)); err != nil {
		return true, ctrl.Result{}, err
	}
	for i := range ds.Items {
		d := &ds.Items[i]
		if !ownedLabDevice(l, d) {
			continue
		}
		base := d.DeepCopy()
		if d.Spec.State != nil {
			d.Spec.State.CaptureRequest = nil
		}
		if !reflect.DeepEqual(base.Spec, d.Spec) {
			if err := r.Patch(ctx, d, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return true, ctrl.Result{}, err
			}
		}
		if d.Spec.Type == lab.DeviceTypeContainer {
			orig := d.DeepCopy()
			// Drop only independently certified retired identities. Unknown old
			// runtime remains held across start and cannot be silently replaced.
			var retained []lab.OwnedRuntimeIdentity
			for _, id := range d.Status.RuntimeInventory {
				if !runtimeRowsReleased([]lab.OwnedRuntimeIdentity{id}, d.Status.RuntimeReports, id.OwnerUID, id.OperationID, id.Revision) {
					retained = append(retained, id)
				}
			}
			d.Status.RuntimeInventory = retained
			now := metav1.Now()
			d.Status.Scheduling = &lab.PodSchedule{State: lab.PodQueued, QueuedAt: &now}
			if err := r.Status().Patch(ctx, d, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
				return true, ctrl.Result{}, err
			}
		}
	}
	next := &lab.LabLifecycleStatus{ObservedState: "Starting", OperationID: l.Spec.Lifecycle.OperationID, Revision: l.Spec.Lifecycle.Revision, LabUID: string(l.UID), ObservedGeneration: l.Generation}
	return true, ctrl.Result{RequeueAfter: time.Second}, r.patchLifecycle(ctx, l, next)
}

// Clear all requests on collective failure. Node-agent invalidation precedes
// thaw, so this never deletes or silently releases a successful held runtime.
func (r *LabReconciler) cancelLifecycleCaptures(ctx context.Context, l *lab.Lab) error {
	var ds lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &ds, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	for i := range ds.Items {
		d := &ds.Items[i]
		if !ownedLabDevice(l, d) || d.Spec.State == nil || d.Spec.State.CaptureRequest == nil {
			continue
		}
		req := d.Spec.State.CaptureRequest
		if req.OperationID != l.Spec.Lifecycle.OperationID || req.LifecycleRevision != l.Spec.Lifecycle.Revision {
			continue
		}
		base := d.DeepCopy()
		d.Spec.State.CaptureRequest = nil
		if err := r.Patch(ctx, d, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

// Explicit Start publishes its current operation only after every owned device
// is queued. A Lab watch may reach Device or Scheduler before that preparation.
// Legacy labs without lifecycle retain their original Running behavior.
func labStartPrepared(l *lab.Lab) bool {
	intent := l.Spec.Lifecycle
	if intent == nil || intent.DesiredState != "Running" {
		return true
	}
	observed := l.Status.Lifecycle
	return observed != nil && observed.LabUID == string(l.UID) && observed.OperationID == intent.OperationID && observed.Revision == intent.Revision && (observed.ObservedState == "Starting" || observed.ObservedState == "Running")
}

// Deployment Pods are fenced through both immutable owner UIDs, never labels.
func (r *LabReconciler) ownedRuntimeDevicePod(ctx context.Context, d *lab.Device, p *corev1.Pod) (bool, error) {
	if ownedDevicePod(d, p) {
		return true, nil
	}
	for _, o := range p.OwnerReferences {
		if o.Kind != "ReplicaSet" {
			continue
		}
		var rs appsv1.ReplicaSet
		if e := r.lifecycleReader().Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: o.Name}, &rs); e != nil {
			return false, client.IgnoreNotFound(e)
		}
		if rs.UID != o.UID {
			continue
		}
		for _, owner := range rs.OwnerReferences {
			if owner.Kind != "Deployment" {
				continue
			}
			var dep appsv1.Deployment
			if e := r.lifecycleReader().Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: owner.Name}, &dep); e != nil {
				return false, client.IgnoreNotFound(e)
			}
			if dep.UID != owner.UID {
				continue
			}
			for _, parent := range dep.OwnerReferences {
				if parent.Kind == "Device" && parent.UID == d.UID && parent.Name == d.Name {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
