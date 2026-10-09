package devicestate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func captureKubeRig(t *testing.T) (*KubeCluster, client.Client, PodInfo, api.DeviceCaptureRequest) {
	d := stateDevice()
	p := statePod("lab-web-3", "node-a", corev1.PodRunning)
	p.UID = "pod-u"
	d.Status.PodName = p.Name
	req := api.DeviceCaptureRequest{OperationID: "op", LifecycleRevision: 1, PodUID: "pod-u", PodResourceVersion: "1", Epoch: 2, Incarnation: 3, DeadlineSeconds: 30}
	d.Spec.State.CaptureRequest = &req
	k, c := kubeRig(t, d, p)
	lab := &api.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "ns"}, Spec: api.LabSpec{Lifecycle: &api.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Required"}}}
	if err := c.Create(context.Background(), lab); err != nil {
		t.Fatal(err)
	}
	return k, c, podInfo(p, d), req
}
func TestCaptureGuardCurrentRVAndInvalidation(t *testing.T) {
	k, c, p, req := captureKubeRig(t)
	ctx := context.Background()
	if err := k.SetCaptureGuard(ctx, p, req, "boot-a"); err != nil {
		t.Fatal(err)
	}
	result := captureResult(req, "boot-a")
	result.Result = "Succeeded"
	result.Quiesced = true
	result.Image = "registry/lab/ns/lab/web@sha256:abc"
	if err := k.RecordCapture(ctx, p, result); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	key := types.NamespacedName{Namespace: "ns", Name: p.Pod}
	if err := c.Get(ctx, key, &pod); err != nil {
		t.Fatal(err)
	}
	// An unrelated kubelet/RV update is allowed while the identity guard remains.
	before := pod.DeepCopy()
	pod.Annotations["kubelet-audit"] = "changed"
	if err := c.Patch(ctx, &pod, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	if !CaptureGuardMatches(&pod, &req, &result) {
		t.Fatal("capture-time RV incorrectly became the deletion fence")
	}
	failed := result
	failed.Result = "Failed"
	failed.GuardState = "Invalidated"
	failed.Quiesced = false
	if err := k.InvalidateCapture(ctx, p, failed); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &pod); err != nil {
		t.Fatal(err)
	}
	if CaptureGuardMatches(&pod, &req, &result) || pod.Annotations[CaptureGuardAnnotation] != "" {
		t.Fatal("invalidated success remains authoritative")
	}
	var d api.Device
	if err := c.Get(ctx, p.Device, &d); err != nil {
		t.Fatal(err)
	}
	if d.Status.State.Image != "registry/lab/ns/lab/web@sha256:abc" || d.Status.State.ExitSnapshotPod != "" {
		t.Fatal("invalidation destroyed latest image or used exit marker")
	}
}
func TestCaptureGuardFencesNewStartAndForeignBoot(t *testing.T) {
	k, c, p, req := captureKubeRig(t)
	ctx := context.Background()
	if err := k.SetCaptureGuard(ctx, p, req, "boot-a"); err != nil {
		t.Fatal(err)
	}
	result := captureResult(req, "boot-b")
	result.Result = "Succeeded"
	result.Quiesced = true
	if err := k.RecordCapture(ctx, p, result); !errors.Is(err, ErrStale) {
		t.Fatalf("foreign boot accepted: %v", err)
	}
	var lab api.Lab
	key := types.NamespacedName{Namespace: "ns", Name: "lab"}
	_ = c.Get(ctx, key, &lab)
	lab.Spec.Lifecycle = &api.LabLifecycleSpec{DesiredState: "Running", OperationID: "start", Revision: 2}
	if err := c.Update(ctx, &lab); err != nil {
		t.Fatal(err)
	}
	result.NodeAgentEpoch = "boot-a"
	if err := k.RecordCapture(ctx, p, result); !errors.Is(err, ErrStale) {
		t.Fatalf("new start did not fence capture: %v", err)
	}
}
func TestCaptureGuardRequiresExactIncarnation(t *testing.T) {
	req := api.DeviceCaptureRequest{OperationID: "op", LifecycleRevision: 1, PodUID: "u", Epoch: 2, Incarnation: 3}
	r := captureResult(req, "boot-a")
	r.Result = "Succeeded"
	r.Quiesced = true
	b, _ := json.Marshal(captureResult(req, "boot-a"))
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "u", Annotations: map[string]string{CaptureGuardAnnotation: string(b), names.AnnotationStateEpoch: "2", names.AnnotationStateIncarnation: "3"}}}
	if !CaptureGuardMatches(p, &req, &r) {
		t.Fatal("valid guard rejected")
	}
	r.Incarnation = 4
	if CaptureGuardMatches(p, &req, &r) {
		t.Fatal("wrong incarnation accepted")
	}
}

func TestCaptureGuardDeletePreconditionsUseCurrentPod(t *testing.T) {
	req := api.DeviceCaptureRequest{OperationID: "op", LifecycleRevision: 1, PodUID: "u", PodResourceVersion: "capture-rv", Epoch: 2, Incarnation: 3}
	result := captureResult(req, "boot-a")
	result.Result = "Succeeded"
	result.Quiesced = true
	b, _ := json.Marshal(captureResult(req, "boot-a"))
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "u", ResourceVersion: "current-rv", Annotations: map[string]string{CaptureGuardAnnotation: string(b), names.AnnotationStateEpoch: "2", names.AnnotationStateIncarnation: "3"}}}
	preconditions, err := CaptureDeletePreconditions(p, &req, &result)
	if err != nil || preconditions == nil || *preconditions.UID != "u" || *preconditions.ResourceVersion != "current-rv" {
		t.Fatalf("unsafe delete fence: %+v %v", preconditions, err)
	}
	delete(p.Annotations, CaptureGuardAnnotation)
	if _, err := CaptureDeletePreconditions(p, &req, &result); !errors.Is(err, ErrStale) {
		t.Fatalf("invalidated guard yielded deletion fence: %v", err)
	}
}
