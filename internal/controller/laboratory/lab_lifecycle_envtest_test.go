package laboratory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type lifecycleConflictClient struct {
	client.Client
	mutate    bool
	conflicts int
}

func (c *lifecycleConflictClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.mutate {
		c.mutate = false
		var pod corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &pod); err != nil {
			return err
		}
		base := pod.DeepCopy()
		pod.Annotations["unrelated"] = "changed-between-read-and-delete"
		if err := c.Patch(ctx, &pod, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	err := c.Client.Delete(ctx, obj, opts...)
	if apierrors.IsConflict(err) {
		c.conflicts++
	}
	return err
}
func TestLifecycleRequiredGuardConflictRereadsCurrentAPIServerPod(t *testing.T) {
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	e := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir()}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lifecycle"}}); err != nil {
		t.Fatal(err)
	}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "lifecycle"}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Required"}, Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Persistence: &lab.DevicePersistence{Enabled: true}}}}}
	if err := c.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "a-web", Namespace: l.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: l.Name, UID: l.UID}}}, Spec: lab.DeviceSpec{LabRef: l.Name, Name: "web", Type: lab.DeviceTypeContainer, Image: "nginx:alpine", Code: "fixed", State: &lab.DeviceStateSpec{Enabled: true, MaxLayers: 1}}}
	if err := c.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "held-pod", Namespace: l.Namespace, Annotations: map[string]string{names.AnnotationStateEpoch: "1", names.AnnotationStateIncarnation: "1"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Device", Name: d.Name, UID: d.UID}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "device", Image: "nginx:alpine"}}, NodeName: "node", TerminationGracePeriodSeconds: &zero}}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	req := &lab.DeviceCaptureRequest{OperationID: "op", LifecycleRevision: 1, PodUID: string(p.UID), PodResourceVersion: p.ResourceVersion, Epoch: 1, Incarnation: 1, DeadlineSeconds: 300, CommitNodeAgentEpoch: "node-boot"}
	result := &lab.DeviceCaptureResult{OperationID: "op", LifecycleRevision: 1, PodUID: string(p.UID), PodResourceVersion: p.ResourceVersion, Epoch: 1, Incarnation: 1, NodeAgentEpoch: "node-boot", Result: "Succeeded", GuardState: "Held", Committed: true, Quiesced: true, Image: "nginx:alpine"}
	d.Spec.State.CaptureRequest = req
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.Status.State = &lab.DeviceStateStatus{Epoch: 1, Incarnation: 1, Capture: result}
	d.Status.NodeName = p.Spec.NodeName
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{lifecycleNativeReport(l, d, p)}
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	base := p.DeepCopy()
	p.Annotations[devicestate.CaptureGuardAnnotation] = string(raw)
	if err := c.Patch(ctx, p, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	wrapped := &lifecycleConflictClient{Client: c, mutate: true}
	r := &LabReconciler{Client: wrapped, Reader: c, Scheme: scheme, RequiredSnapshotAvailable: true}
	_, requeue, err := r.reconcileLifecycle(ctx, l)
	if err != nil || requeue.RequeueAfter == 0 || wrapped.conflicts != 1 {
		t.Fatal("actual stale-RV delete did not conflict/requeue", requeue, wrapped.conflicts, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil || p.DeletionTimestamp != nil {
		t.Fatal("conflicted runtime deleted", err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); !apierrors.IsNotFound(err) {
		t.Fatal("fresh current-RV guarded retry did not delete", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle.ObservedState != "Unknown" {
		t.Fatal("API deletion fabricated native stop acknowledgement")
	}
}
