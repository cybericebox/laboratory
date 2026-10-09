package devicestate

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func captureFailurePublicationRig(t *testing.T) (*KubeCluster, client.Client, PodInfo, api.DeviceCaptureResult, *api.Device) {
	t.Helper()
	d := stateDevice()
	d.UID = "device-uid"
	d.OwnerReferences = []metav1.OwnerReference{{Kind: "Lab", Name: "lab", UID: "lab-uid"}}
	p := statePod("lab-web-3", "node-a", corev1.PodRunning)
	p.UID = "pod-uid"
	controller := true
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID, Controller: &controller}}
	d.Status.PodName = p.Name
	req := api.DeviceCaptureRequest{OperationID: "current-stop", LifecycleRevision: 3, PodUID: string(p.UID), PodResourceVersion: "current-audit-rv", Epoch: 2, Incarnation: 3, DeadlineSeconds: 30}
	d.Spec.State.CaptureRequest = &req
	at := metav1.NewTime(time.Now().Add(-time.Hour))
	d.Status.State.Image, d.Status.State.SnapshotAt, d.Status.State.SizeBytes, d.Status.State.Layers = "registry/lab/ns/lab/web@sha256:retained", &at, 42, 2
	d.Status.State.Capture = &api.DeviceCaptureResult{OperationID: "old-stop", LifecycleRevision: 2, PodUID: string(p.UID), PodResourceVersion: "old-audit-rv", Epoch: 2, Incarnation: 3, NodeAgentEpoch: "old-boot", Result: "Failed", GuardState: "Invalidated", Error: "old failure"}
	k, c := kubeRig(t, d, p)
	l := &api.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "ns", UID: "lab-uid", Generation: 4}, Spec: api.LabSpec{Lifecycle: &api.LabLifecycleSpec{DesiredState: "Stopped", OperationID: req.OperationID, Revision: req.LifecycleRevision, SnapshotMode: "Required"}}}
	if err := c.Create(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	info := podInfo(p, d)
	failure := captureResult(req, "current-boot")
	failure.Result, failure.GuardState, failure.Error = "Failed", "Invalidated", "snapshot deferred: pushed recently"
	return k, c, info, failure, d
}

func TestRequiredFailurePublicationRefusesSupersededAuthority(t *testing.T) {
	changes := map[string]func(*testing.T, client.Client, PodInfo, *api.DeviceCaptureResult){
		"old operation":            func(_ *testing.T, _ client.Client, _ PodInfo, r *api.DeviceCaptureResult) { r.OperationID = "old-stop" },
		"wrong pod uid":            func(_ *testing.T, _ client.Client, _ PodInfo, r *api.DeviceCaptureResult) { r.PodUID = "replacement" },
		"wrong result epoch":       func(_ *testing.T, _ client.Client, _ PodInfo, r *api.DeviceCaptureResult) { r.Epoch++ },
		"wrong result incarnation": func(_ *testing.T, _ client.Client, _ PodInfo, r *api.DeviceCaptureResult) { r.Incarnation++ },
		"new request": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			d.Spec.State.CaptureRequest.OperationID = "newer-stop"
			d.Spec.State.CaptureRequest.LifecycleRevision = 4
			if err := c.Update(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
		},
		"cancelled request": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			d.Spec.State.CaptureRequest = nil
			if err := c.Update(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
		},
		"new start": func(t *testing.T, c client.Client, _ PodInfo, _ *api.DeviceCaptureResult) {
			var l api.Lab
			_ = c.Get(context.Background(), types.NamespacedName{Name: "lab", Namespace: "ns"}, &l)
			l.Spec.Lifecycle = &api.LabLifecycleSpec{DesiredState: "Running", OperationID: "start", Revision: 4}
			if err := c.Update(context.Background(), &l); err != nil {
				t.Fatal(err)
			}
		},
		"replacement device": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			if err := c.Delete(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
			d.ResourceVersion = ""
			d.UID = "replacement"
			if err := c.Create(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
		},
		"replacement lab": func(t *testing.T, c client.Client, _ PodInfo, _ *api.DeviceCaptureResult) {
			var l api.Lab
			_ = c.Get(context.Background(), types.NamespacedName{Name: "lab", Namespace: "ns"}, &l)
			if err := c.Delete(context.Background(), &l); err != nil {
				t.Fatal(err)
			}
			l.ResourceVersion = ""
			l.UID = "replacement"
			if err := c.Create(context.Background(), &l); err != nil {
				t.Fatal(err)
			}
		},
		"foreign guard": func(t *testing.T, c client.Client, p PodInfo, r *api.DeviceCaptureResult) {
			var pod corev1.Pod
			_ = c.Get(context.Background(), types.NamespacedName{Name: p.Pod, Namespace: p.Device.Namespace}, &pod)
			foreign := *r
			foreign.NodeAgentEpoch = "foreign-boot"
			raw, _ := json.Marshal(foreign)
			pod.Annotations[CaptureGuardAnnotation] = string(raw)
			if err := c.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
		},
		"noncontroller pod owner": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var pod corev1.Pod
			_ = c.Get(context.Background(), types.NamespacedName{Name: p.Pod, Namespace: p.Device.Namespace}, &pod)
			pod.OwnerReferences[0].Controller = nil
			if err := c.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
		},
		"ambiguous pod owner": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var pod corev1.Pod
			_ = c.Get(context.Background(), types.NamespacedName{Name: p.Pod, Namespace: p.Device.Namespace}, &pod)
			pod.OwnerReferences = append(pod.OwnerReferences, pod.OwnerReferences[0])
			if err := c.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
		},
		"foreign lab owner": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			d.OwnerReferences[0].UID = "foreign"
			if err := c.Update(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
		},
		"ambiguous lab owner": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			d.OwnerReferences = append(d.OwnerReferences, d.OwnerReferences[0])
			if err := c.Update(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
		},
		"deleting pod": func(t *testing.T, c client.Client, p PodInfo, _ *api.DeviceCaptureResult) {
			var pod corev1.Pod
			_ = c.Get(context.Background(), types.NamespacedName{Name: p.Pod, Namespace: p.Device.Namespace}, &pod)
			pod.Finalizers = []string{"hold"}
			if err := c.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
		},
		"same request foreign boot": func(t *testing.T, c client.Client, p PodInfo, r *api.DeviceCaptureResult) {
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			success := *r
			success.NodeAgentEpoch = "newer-boot"
			success.Result = "Succeeded"
			success.GuardState = "Held"
			success.Quiesced = true
			d.Status.State.Capture = &success
			if err := c.Status().Update(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			k, c, p, result, _ := captureFailurePublicationRig(t)
			change(t, c, p, &result)
			var before api.Device
			if err := c.Get(context.Background(), p.Device, &before); err != nil {
				t.Fatal(err)
			}
			err := k.InvalidateCapture(context.Background(), p, result)
			if !errors.Is(err, ErrStale) && !errors.Is(err, ErrDeleting) {
				t.Fatalf("superseded authority accepted: %v", err)
			}
			var after api.Device
			if err := c.Get(context.Background(), p.Device, &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Status.State, after.Status.State) {
				t.Fatal("stale failure changed status or retained snapshot")
			}
		})
	}
}

func TestRequiredFailurePublicationConflictRefetchAndCancellation(t *testing.T) {
	for _, mode := range []string{"retry", "new request", "new held capture"} {
		t.Run(mode, func(t *testing.T) {
			k, c, p, result, _ := captureFailurePublicationRig(t)
			patches := 0
			k.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				patches++
				if patches == 1 {
					if mode == "new request" {
						var d api.Device
						_ = c.Get(ctx, p.Device, &d)
						d.Spec.State.CaptureRequest.OperationID = "newer-stop"
						d.Spec.State.CaptureRequest.LifecycleRevision = 4
						if err := c.Update(ctx, &d); err != nil {
							return err
						}
					}
					if mode == "new held capture" {
						var d api.Device
						_ = c.Get(ctx, p.Device, &d)
						success := result
						success.Result = "Succeeded"
						success.GuardState = "Held"
						success.Quiesced = true
						d.Status.State.Capture = &success
						if err := c.Status().Update(ctx, &d); err != nil {
							return err
						}
						var pod corev1.Pod
						_ = c.Get(ctx, types.NamespacedName{Name: p.Pod, Namespace: p.Device.Namespace}, &pod)
						raw, _ := json.Marshal(success)
						pod.Annotations[CaptureGuardAnnotation] = string(raw)
						if err := c.Update(ctx, &pod); err != nil {
							return err
						}
					}
					return apierrors.NewConflict(api.Resource("devices"), obj.GetName(), errors.New("status race"))
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			}})
			err := k.InvalidateCapture(context.Background(), p, result)
			if mode == "retry" {
				if err != nil || patches != 2 {
					t.Fatalf("current failure did not retry conflict: %v patches=%d", err, patches)
				}
			} else if !errors.Is(err, ErrStale) {
				t.Fatalf("conflict bypassed fresh request/guard checks: %v", err)
			}
		})
	}
	for _, mode := range []string{"cancelled", "restart", "new start"} {
		t.Run(mode, func(t *testing.T) {
			k, c, p, _, original := captureFailurePublicationRig(t)
			failed := *original.Status.State.Capture
			failed.Error = mode
			var d api.Device
			_ = c.Get(context.Background(), p.Device, &d)
			d.Spec.State.CaptureRequest = nil
			if err := c.Update(context.Background(), &d); err != nil {
				t.Fatal(err)
			}
			var pod corev1.Pod
			_ = c.Get(context.Background(), types.NamespacedName{Name: p.Pod, Namespace: p.Device.Namespace}, &pod)
			held := failed
			held.Result = "Succeeded"
			held.GuardState = "Held"
			held.Quiesced = true
			raw, _ := json.Marshal(held)
			pod.Annotations[CaptureGuardAnnotation] = string(raw)
			if err := c.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			if mode == "new start" {
				var l api.Lab
				_ = c.Get(context.Background(), types.NamespacedName{Name: "lab", Namespace: "ns"}, &l)
				l.Spec.Lifecycle = &api.LabLifecycleSpec{DesiredState: "Running", OperationID: "start", Revision: 4}
				if err := c.Update(context.Background(), &l); err != nil {
					t.Fatal(err)
				}
			}
			if err := k.InvalidateCapture(context.Background(), p, failed); err != nil {
				t.Fatalf("matching old hold cannot invalidate on %s: %v", mode, err)
			}
			if err := c.Get(context.Background(), p.Device, &d); err != nil {
				t.Fatal(err)
			}
			if !sameCapture(*d.Status.State.Capture, failed) || d.Status.State.Capture.Error != mode || d.Status.State.Image != original.Status.State.Image {
				t.Fatal("old cancellation damaged newer capture/snapshot")
			}
		})
	}
}

func TestRequiredFailurePublicationCurrentRequestReplacesOldCapture(t *testing.T) {
	for _, guard := range []bool{false, true} {
		t.Run(map[bool]string{false: "already absent guard", true: "owned current guard"}[guard], func(t *testing.T) {
			k, c, p, result, original := captureFailurePublicationRig(t)
			if guard {
				if err := k.SetCaptureGuard(context.Background(), p, *original.Spec.State.CaptureRequest, result.NodeAgentEpoch); err != nil {
					t.Fatal(err)
				}
			}
			if err := k.InvalidateCapture(context.Background(), p, result); err != nil {
				t.Fatal(err)
			}
			var actual api.Device
			if err := c.Get(context.Background(), p.Device, &actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.Status.State.Capture, &result) {
				t.Fatalf("current failure silently dropped behind old capture: %+v", actual.Status.State.Capture)
			}
			st, prior := actual.Status.State, original.Status.State
			if st.Image != prior.Image || !reflect.DeepEqual(st.SnapshotAt, prior.SnapshotAt) || st.SizeBytes != prior.SizeBytes || st.Layers != prior.Layers {
				t.Fatalf("failure damaged retained snapshot: %+v", st)
			}
			var pod corev1.Pod
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: p.Device.Namespace, Name: p.Pod}, &pod); err != nil {
				t.Fatal(err)
			}
			if pod.Annotations[CaptureGuardAnnotation] != "" {
				t.Fatal("failure retained deletion authority")
			}
		})
	}
}
