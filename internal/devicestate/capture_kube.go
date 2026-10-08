package devicestate

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func requestOf(r api.DeviceCaptureResult) api.DeviceCaptureRequest {
	return api.DeviceCaptureRequest{OperationID: r.OperationID, LifecycleRevision: r.LifecycleRevision, PodUID: r.PodUID, PodResourceVersion: r.PodResourceVersion, Epoch: r.Epoch, Incarnation: r.Incarnation}
}
func sameCapture(a, b api.DeviceCaptureResult) bool {
	return a.OperationID == b.OperationID && a.LifecycleRevision == b.LifecycleRevision && a.PodUID == b.PodUID && a.Epoch == b.Epoch && a.Incarnation == b.Incarnation && a.NodeAgentEpoch == b.NodeAgentEpoch
}

// CaptureGuardMatches is the controller's barrier predicate. Call it only on
// direct-read current Pod and Device. Delete uses that current Pod UID/RV, not
// the result's capture-time resourceVersion (which is audit information).
func CaptureGuardMatches(p *corev1.Pod, req *api.DeviceCaptureRequest, result *api.DeviceCaptureResult) bool {
	if p == nil || req == nil || result == nil || result.Result != "Succeeded" || !result.Quiesced || result.GuardState != "Held" || result.NodeAgentEpoch == "" || string(p.UID) != req.PodUID || p.DeletionTimestamp != nil || annotationInt(p, names.AnnotationStateEpoch) != req.Epoch || annotationInt(p, names.AnnotationStateIncarnation) != req.Incarnation {
		return false
	}
	var guard api.DeviceCaptureResult
	if json.Unmarshal([]byte(p.Annotations[CaptureGuardAnnotation]), &guard) != nil {
		return false
	}
	expected := captureResult(PodInfo{}, *req, result.NodeAgentEpoch)
	return sameCapture(expected, *result) && sameCapture(guard, *result) && guard.GuardState == "Held"
}

func (k *KubeCluster) captureCurrent(ctx context.Context, p PodInfo, req api.DeviceCaptureRequest) (*api.Device, error) {
	var d api.Device
	if err := k.Reader.Get(ctx, p.Device, &d); err != nil {
		return nil, err
	}
	if !d.Spec.StateEnabled() || d.Status.State == nil || d.Spec.State.CaptureRequest == nil || d.Status.State.Epoch != req.Epoch || d.Status.State.Incarnation != req.Incarnation {
		return nil, ErrStale
	}
	actual := *d.Spec.State.CaptureRequest
	actual.DeadlineSeconds = req.DeadlineSeconds
	// Commit control is additive; it does not supersede capture identity.
	actual.CommitNodeAgentEpoch = req.CommitNodeAgentEpoch
	if !reflect.DeepEqual(actual, req) {
		return nil, ErrStale
	}
	var lab api.Lab
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Spec.LabRef}, &lab); err != nil {
		return nil, err
	}
	if lab.Spec.Lifecycle == nil || lab.Spec.Lifecycle.DesiredState != "Stopped" || lab.Spec.Lifecycle.OperationID != req.OperationID || lab.Spec.Lifecycle.Revision != req.LifecycleRevision {
		return nil, ErrStale
	}
	return &d, nil
}

func (k *KubeCluster) currentCapturePod(ctx context.Context, p PodInfo) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: p.Device.Namespace, Name: p.Pod}, &pod); err != nil {
		return nil, err
	}
	if string(pod.UID) != p.UID || pod.Spec.NodeName != k.NodeName || pod.Annotations[names.AnnotationStateDevice] != p.Device.Name || annotationInt(&pod, names.AnnotationStateEpoch) != p.Epoch || annotationInt(&pod, names.AnnotationStateIncarnation) != p.Incarnation {
		return nil, ErrStale
	}
	return &pod, nil
}

func (k *KubeCluster) SetCaptureGuard(ctx context.Context, p PodInfo, req api.DeviceCaptureRequest, boot string) error {
	if _, err := k.captureCurrent(ctx, p, req); err != nil {
		return err
	}
	return k.patchGuard(ctx, p, func(pod *corev1.Pod) error {
		if pod.DeletionTimestamp != nil {
			return ErrDeleting
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		result := captureResult(p, req, boot)
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		pod.Annotations[CaptureGuardAnnotation] = string(b)
		return nil
	})
}

func (k *KubeCluster) patchGuard(ctx context.Context, p PodInfo, mutate func(*corev1.Pod) error) error {
	var err error
	for i := 0; i < 5; i++ {
		pod, e := k.currentCapturePod(ctx, p)
		if e != nil {
			return e
		}
		before := pod.DeepCopy()
		if err = mutate(pod); err != nil {
			return err
		}
		err = k.Client.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

func (k *KubeCluster) RecordCapture(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) error {
	for i := 0; i < 5; i++ {
		d, err := k.captureCurrent(ctx, p, requestOf(result))
		if err != nil {
			return err
		}
		pod, err := k.currentCapturePod(ctx, p)
		if err != nil {
			return err
		}
		if result.Result == "Succeeded" && !CaptureGuardMatches(pod, d.Spec.State.CaptureRequest, &result) {
			return ErrStale
		}
		before := d.DeepCopy()
		d.Status.State.Capture = result.DeepCopy()
		if result.Result == "Succeeded" {
			d.Status.State.Image = result.Image
			if !strings.Contains(result.Image, "/"+p.Repo+"@") {
				d.Status.State.Image = ""
			}
			d.Status.State.SnapshotAt = result.SnapshotAt
			d.Status.State.SizeBytes = result.SizeBytes
			d.Status.State.Layers = p.capturedLayers
			d.Status.State.Warning = ""
		}
		err = k.Client.Status().Patch(ctx, d, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return ErrStale
}

// InvalidateCapture first changes the current Pod RV. Any deletion using the
// previous guard/RV now conflicts. A deleting Pod is never modified or thawed.
func (k *KubeCluster) InvalidateCapture(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) error {
	err := k.patchGuard(ctx, p, func(pod *corev1.Pod) error {
		if pod.DeletionTimestamp != nil {
			return ErrDeleting
		}
		var guard api.DeviceCaptureResult
		if json.Unmarshal([]byte(pod.Annotations[CaptureGuardAnnotation]), &guard) == nil && !sameCapture(guard, result) {
			return ErrStale
		}
		delete(pod.Annotations, CaptureGuardAnnotation)
		return nil
	})
	if err != nil {
		return err
	}
	// Status write may fail/stale after the guard is removed; that never restores
	// authority to delete. Still fail closed and retry rather than thaw on API error.
	return k.update(ctx, p, true, func(st *api.DeviceStateStatus) {
		if st.Capture == nil || sameCapture(*st.Capture, result) {
			st.Capture = result.DeepCopy()
		}
	})
}

func (k *KubeCluster) CheckCapture(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) (bool, error) {
	pod, err := k.currentCapturePod(ctx, p)
	if err != nil {
		return false, err
	}
	if pod.DeletionTimestamp != nil {
		return true, nil
	}
	if _, err = k.captureCurrent(ctx, p, requestOf(result)); err != nil {
		if err == ErrStale {
			return false, ErrStale
		}
		return false, err
	}
	return false, nil
}

// CaptureDeletePreconditions supplies the exact current Pod fence after the
// caller direct-reads the current lifecycle, Device and Pod. On Conflict,
// reread those objects and repeat; never substitute the capture-time RV.
func CaptureDeletePreconditions(p *corev1.Pod, req *api.DeviceCaptureRequest, result *api.DeviceCaptureResult) (*metav1.Preconditions, error) {
	if !CaptureGuardMatches(p, req, result) || p.ResourceVersion == "" {
		return nil, ErrStale
	}
	uid, rv := p.UID, p.ResourceVersion
	return &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, nil
}

func (k *KubeCluster) CaptureCommitRequested(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) (bool, error) {
	d, err := k.captureCurrent(ctx, p, requestOf(result))
	if err != nil {
		return false, err
	}
	pod, err := k.currentCapturePod(ctx, p)
	if err != nil {
		return false, err
	}
	if !CaptureGuardMatches(pod, d.Spec.State.CaptureRequest, &result) {
		return false, ErrStale
	}
	return d.Spec.State.CaptureRequest.CommitNodeAgentEpoch == result.NodeAgentEpoch && result.NodeAgentEpoch != "", nil
}
func (k *KubeCluster) AcknowledgeCaptureCommit(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) error {
	if !result.Committed {
		return ErrStale
	}
	wanted, err := k.CaptureCommitRequested(ctx, p, result)
	if err != nil {
		return err
	}
	if !wanted {
		return ErrStale
	}
	d, err := k.captureCurrent(ctx, p, requestOf(result))
	if err != nil {
		return err
	}
	currentPod, err := k.currentCapturePod(ctx, p)
	if err != nil {
		return err
	}
	if d.Status.State.Capture != nil && d.Status.State.Capture.Committed && sameCapture(*d.Status.State.Capture, result) && CaptureCommittedGuardMatches(currentPod, d.Spec.State.CaptureRequest, &result) {
		return nil
	}
	if err := k.patchGuard(ctx, p, func(pod *corev1.Pod) error {
		if pod.DeletionTimestamp != nil {
			return ErrDeleting
		}
		if !CaptureGuardMatches(pod, &api.DeviceCaptureRequest{OperationID: result.OperationID, LifecycleRevision: result.LifecycleRevision, PodUID: result.PodUID, Epoch: result.Epoch, Incarnation: result.Incarnation}, &result) {
			return ErrStale
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		pod.Annotations[CaptureGuardAnnotation] = string(raw)
		return nil
	}); err != nil {
		return err
	}
	for i := 0; i < 5; i++ {
		d, err := k.captureCurrent(ctx, p, requestOf(result))
		if err != nil {
			return err
		}
		if d.Spec.State.CaptureRequest.CommitNodeAgentEpoch != result.NodeAgentEpoch {
			return ErrStale
		}
		pod, err := k.currentCapturePod(ctx, p)
		if err != nil {
			return err
		}
		if !CaptureCommittedGuardMatches(pod, d.Spec.State.CaptureRequest, &result) {
			return ErrStale
		}
		before := d.DeepCopy()
		if d.Status.State.Capture == nil || !sameCapture(*d.Status.State.Capture, result) {
			return ErrStale
		}
		d.Status.State.Capture = result.DeepCopy()
		err = k.Client.Status().Patch(ctx, d, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return ErrStale
}
func CaptureCommittedGuardMatches(p *corev1.Pod, req *api.DeviceCaptureRequest, result *api.DeviceCaptureResult) bool {
	if !CaptureGuardMatches(p, req, result) || !result.Committed || req.CommitNodeAgentEpoch != result.NodeAgentEpoch {
		return false
	}
	var guard api.DeviceCaptureResult
	return json.Unmarshal([]byte(p.Annotations[CaptureGuardAnnotation]), &guard) == nil && guard.Committed
}
func CaptureCommittedDeletePreconditions(p *corev1.Pod, req *api.DeviceCaptureRequest, result *api.DeviceCaptureResult) (*metav1.Preconditions, error) {
	if !CaptureCommittedGuardMatches(p, req, result) {
		return nil, ErrStale
	}
	return CaptureDeletePreconditions(p, req, result)
}
