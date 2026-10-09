package devicestate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The client can observe an eligible snapshot before a newer ordinary push wins
// the lifecycle preparation race. Required must reserve the next eligible push.
func TestRequiredCaptureWaitsForOrdinaryPushThroughSyncAndWatch(t *testing.T) {
	for _, dispatch := range []string{"direct", "sync"} {
		t.Run(dispatch, func(t *testing.T) {
			r := newRig(t, 5*time.Millisecond, 1<<20)
			r.e.MinPushInterval = 200 * time.Millisecond
			r.e.Poll = 5 * time.Millisecond
			req := captureRequest(r)
			r.cl.mu.Lock()
			r.cl.pods = []PodInfo{r.pod}
			r.cl.mu.Unlock()
			r.rt.setDiff(tarOf(map[string]string{"work": "ordinary saved"}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.e.Sync(ctx)
			defer r.e.stopAll()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if records, _, _ := r.cl.snapshot(); len(records) == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Watch/Debounce did not publish the ordinary snapshot")
				}
				time.Sleep(time.Millisecond)
			}
			r.e.mu.Lock()
			tr := r.e.tracked[r.pod.ContainerID]
			r.e.mu.Unlock()
			tr.mu.Lock()
			last := tr.pushes[len(tr.pushes)-1].at
			tr.mu.Unlock()
			r.rt.setDiff(tarOf(map[string]string{"work": "required changed"}))
			type outcome struct {
				result api.DeviceCaptureResult
				err    error
			}
			done := make(chan outcome, 1)
			if dispatch == "direct" {
				go func() {
					result, err := r.e.CaptureRequired(ctx, r.pod, req)
					done <- outcome{result, err}
				}()
			} else {
				p := r.pod
				p.CaptureRequest = &req
				r.cl.mu.Lock()
				r.cl.pods = []PodInfo{p}
				r.cl.mu.Unlock()
				r.e.Sync(ctx)
				go func() {
					r.e.captureWorkers.Wait()
					r.cl.mu.Lock()
					result := r.cl.capture
					r.cl.mu.Unlock()
					done <- outcome{result: result}
				}()
			}
			// An actual watcher change must not take another ordinary turn while
			// the Required request reserves the residual interval.
			if err := os.WriteFile(filepath.Join(r.rt.upper, "change"), []byte("required"), 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				t.Fatalf("Required finished before the residual interval: %+v %v", got.result, got.err)
			case <-time.After(30 * time.Millisecond):
			}
			r.rt.mu.Lock()
			held := r.rt.held
			r.rt.mu.Unlock()
			r.cl.mu.Lock()
			guard := r.cl.guard
			r.cl.mu.Unlock()
			if held || guard.OperationID != "" {
				t.Fatal("residual interval must wait before guard publication and freeze")
			}
			journals, err := filepath.Glob(filepath.Join(r.e.WorkDir, "capture-holds", "*.json"))
			if err != nil || len(journals) != 0 {
				t.Fatalf("residual wait created a hold journal: %v %v", journals, err)
			}
			select {
			case got := <-done:
				if got.err != nil || got.result.Result != "Succeeded" || !got.result.Quiesced || got.result.GuardState != "Held" {
					t.Fatalf("Required did not capture after the interval: %+v %v", got.result, got.err)
				}
				if got.result.SnapshotAt == nil || got.result.SnapshotAt.Time.Before(last.Add(r.e.MinPushInterval)) {
					t.Fatal("Required published before the actual ordinary completion budget expired")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Required did not finish within its deadline")
			}
			if records, _, exits := r.cl.snapshot(); len(records) != 1 || len(exits) != 0 {
				t.Fatalf("ordinary persistence or exit advanced during Required: %d %v", len(records), exits)
			}
		})
	}
}

func assertNoIntervalHold(t *testing.T, r *rig) {
	t.Helper()
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.mu.Unlock()
	if held {
		t.Fatal("interval scheduling failure froze the runtime")
	}
	files, err := filepath.Glob(filepath.Join(r.e.WorkDir, "capture-holds", "*.json"))
	if err != nil || len(files) != 0 {
		t.Fatalf("interval scheduling failure retained a hold: %v %v", files, err)
	}
	if records, _, exits := r.cl.snapshot(); len(records) != 1 || len(exits) != 0 {
		t.Fatalf("interval scheduling failure changed persistence or exit: %d %v", len(records), exits)
	}
}

func TestRequiredCaptureIntervalWaitDeadlineAndCancellation(t *testing.T) {
	for _, failure := range []string{"deadline", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			req := captureRequest(r)
			tr := r.track(t)
			defer r.e.stopAll()
			if err := push(t, r, tr, "work", "prior good"); err != nil {
				t.Fatal(err)
			}
			r.e.MinPushInterval = 200 * time.Millisecond
			if failure == "deadline" {
				r.e.MinPushInterval = 2 * time.Second
				req.DeadlineSeconds = 1
			}
			r.rt.setDiff(tarOf(map[string]string{"work": "required changed"}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := r.e.CaptureRequired(ctx, r.pod, req)
				if result.Result != "Failed" || result.Image != "" || result.Quiesced {
					err = errors.New("interval failure acknowledged an image or freeze")
				}
				done <- err
			}()
			if failure == "cancel" {
				select {
				case err := <-done:
					t.Fatalf("capture failed before cancellation: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				cancel()
			}
			select {
			case err := <-done:
				var deferred *errDeferred
				if failure == "cancel" && !errors.Is(err, context.Canceled) || failure == "deadline" && !errors.As(err, &deferred) {
					t.Fatalf("wrong interval failure: %v", err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("an impossible or cancelled wait did not finish promptly")
			}
			r.cl.mu.Lock()
			result, guard := r.cl.capture, r.cl.guard
			r.cl.mu.Unlock()
			if result.Result != "Failed" || guard.OperationID != "" {
				t.Fatalf("interval failure was not published or retained a guard: %+v %+v", result, guard)
			}
			assertNoIntervalHold(t, r)
		})
	}
}

func TestRequiredCaptureIntervalWaitPreservesNoPushWithShortDeadline(t *testing.T) {
	for _, contents := range []string{"unchanged", "empty"} {
		t.Run(contents, func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			req := captureRequest(r)
			req.DeadlineSeconds = 1
			tr := r.track(t)
			defer r.e.stopAll()
			if err := push(t, r, tr, "work", "prior good"); err != nil {
				t.Fatal(err)
			}
			r.e.MinPushInterval = 2 * time.Second
			if contents == "empty" {
				r.rt.setDiff(tarOf(map[string]string{}))
			}
			result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
			if err != nil || result.Result != "Succeeded" || !result.Quiesced || result.GuardState != "Held" {
				t.Fatalf("no-push capture lost its short-deadline success: %+v %v", result, err)
			}
			tr.mu.Lock()
			pushes := len(tr.pushes)
			tr.mu.Unlock()
			if pushes != 1 {
				t.Fatalf("no-push capture published a new registry image: %d", pushes)
			}
		})
	}
}

// Keep the real API authority checks while using the existing fake ordinary
// registry/runtime plumbing. A wait must not weaken any post-wait binding.
type intervalAuthorityCluster struct {
	*fakeCluster
	authority *KubeCluster
}

func (c *intervalAuthorityCluster) SetCaptureGuard(ctx context.Context, p PodInfo, req api.DeviceCaptureRequest, boot string) error {
	return c.authority.SetCaptureGuard(ctx, p, req, boot)
}

func (c *intervalAuthorityCluster) RecordCapture(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) error {
	return c.authority.RecordCapture(ctx, p, result)
}

func (c *intervalAuthorityCluster) InvalidateCapture(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) error {
	return c.authority.InvalidateCapture(ctx, p, result)
}

func (c *intervalAuthorityCluster) CheckCapture(ctx context.Context, p PodInfo, result api.DeviceCaptureResult) (bool, error) {
	return c.authority.CheckCapture(ctx, p, result)
}

func TestRequiredCaptureIntervalWaitRevalidatesAuthorityAndTask(t *testing.T) {
	for _, change := range []string{"cleared request", "new request", "replacement pod", "exited task", "cancelled old request"} {
		t.Run(change, func(t *testing.T) {
			k, c, p, _, original := captureFailurePublicationRig(t)
			req := *original.Spec.State.CaptureRequest
			r := newRig(t, time.Hour, 1<<20)
			p.ContainerID = r.pod.ContainerID
			p.Policy, p.Repo = r.pod.Policy, r.pod.Repo
			r.pod = p
			r.e.NodeAgentEpoch = "current-boot"
			r.cl.mu.Lock()
			ordinaryPod := p
			ordinaryPod.CaptureRequest = nil
			r.cl.pods = []PodInfo{ordinaryPod}
			r.cl.mu.Unlock()
			r.e.Cluster = &intervalAuthorityCluster{fakeCluster: r.cl, authority: k}
			tr := r.track(t)
			defer r.e.stopAll()
			if err := push(t, r, tr, "work", "prior good"); err != nil {
				t.Fatal(err)
			}
			r.e.MinPushInterval = 200 * time.Millisecond
			r.rt.setDiff(tarOf(map[string]string{"work": "required changed"}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := r.e.CaptureRequired(ctx, p, req)
				if result.Result == "Succeeded" || result.Image != "" {
					err = errors.New("superseded interval wait succeeded")
				}
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("capture finished before authority changed during the wait: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			var d api.Device
			if err := c.Get(ctx, p.Device, &d); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "cleared request":
				d.Spec.State.CaptureRequest = nil
			case "new request", "cancelled old request":
				d.Spec.State.CaptureRequest.OperationID = "newer-stop"
				d.Spec.State.CaptureRequest.LifecycleRevision++
			case "replacement pod":
				var pod corev1.Pod
				if err := c.Get(ctx, types.NamespacedName{Namespace: p.Device.Namespace, Name: p.Pod}, &pod); err != nil {
					t.Fatal(err)
				}
				pod.UID = "replacement-pod"
				if err := c.Update(ctx, &pod); err != nil {
					t.Fatal(err)
				}
			case "exited task":
				r.rt.mu.Lock()
				r.rt.gone = true
				r.rt.mu.Unlock()
			}
			if err := c.Update(ctx, &d); err != nil {
				t.Fatal(err)
			}
			if change == "cancelled old request" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("changed authority or task was accepted after the wait")
				}
			case <-time.After(time.Second):
				t.Fatal("changed wait did not finish within its deadline")
			}
			if err := c.Get(context.Background(), p.Device, &d); err != nil {
				t.Fatal(err)
			}
			if d.Status.State.Image != original.Status.State.Image || d.Status.State.Capture.Result == "Succeeded" {
				t.Fatal("changed wait replaced latest saved state or authorized deletion")
			}
			if change != "exited task" && d.Status.State.Capture.OperationID != original.Status.State.Capture.OperationID {
				t.Fatal("stale wait overwrote a different capture receipt")
			}
			var pod corev1.Pod
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: p.Device.Namespace, Name: p.Pod}, &pod); err != nil {
				t.Fatal(err)
			}
			if pod.Annotations[CaptureGuardAnnotation] != "" || pod.DeletionTimestamp != nil {
				t.Fatal("changed wait left a guard or deleted the Pod")
			}
			assertNoIntervalHold(t, r)
		})
	}
}
