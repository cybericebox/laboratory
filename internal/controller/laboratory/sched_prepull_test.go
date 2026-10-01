package laboratory

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestPrepullProgress(t *testing.T) {
	ds := &appsv1.DaemonSet{}
	ds.Generation = 1
	if st := prepullProgress(ds, nil); st.done {
		t.Fatal("status not observed yet: not done")
	}
	ds.Status.ObservedGeneration = 1
	if st := prepullProgress(ds, nil); !st.done || st.desired != 0 {
		t.Fatal("no eligible node: done")
	}
	ds.Status.DesiredNumberScheduled = 2
	pod := func(ready bool) corev1.Pod {
		st := corev1.ContainerStatus{Name: "i0"}
		if ready {
			st.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 127}
		}
		return corev1.Pod{
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "i0"}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{st}},
		}
	}
	st := prepullProgress(ds, []corev1.Pod{pod(true), pod(false)})
	if st.pulled != 1 || st.resolved != 1 || st.desired != 2 || st.done {
		t.Fatalf("progress = %+v", st)
	}
	if st := prepullProgress(ds, []corev1.Pod{pod(true), pod(true)}); !st.done || len(st.failed) != 0 {
		t.Fatalf("all pulled: %+v", st)
	}
	// A pod with no container status yet has pulled nothing.
	if st := prepullProgress(ds, []corev1.Pod{{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "i0"}}}}}); st.pulled != 0 || st.resolved != 0 {
		t.Fatal("pod without statuses")
	}
}

func waitingPod(image, reason string) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "i0", Image: image}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "i0", Image: image, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}},
	}
}

// A pull error resolves the image at once: it is named, and the prepull is done without it.
func TestPrepullProgressGivesUpOnAnImageThatCannotBePulled(t *testing.T) {
	ds := &appsv1.DaemonSet{}
	ds.Generation, ds.Status.ObservedGeneration, ds.Status.DesiredNumberScheduled = 1, 1, 2
	for _, reason := range []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull"} {
		st := prepullProgress(ds, []corev1.Pod{waitingPod("reg/broken:nope", reason), waitingPod("reg/broken:nope", reason)})
		if !st.done || st.pulled != 0 || len(st.failed) != 1 || st.failed[0] != "reg/broken:nope" {
			t.Fatalf("%s: %+v", reason, st)
		}
	}
	// A slow pull (the container is only being created) is still waited for.
	if st := prepullProgress(ds, []corev1.Pod{waitingPod("reg/slow:1", "ContainerCreating"), waitingPod("reg/slow:1", "ContainerCreating")}); st.done {
		t.Fatalf("a slow pull is not a failure: %+v", st)
	}
	// One broken image among good ones: the pod is resolved but not pulled.
	mixed := corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "i0", Image: "good"}, {Name: "i1", Image: "bad"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "i0", ImageID: "good@sha256:1"},
			{Name: "i1", Image: "bad", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
		}},
	}
	ds.Status.DesiredNumberScheduled = 1
	if st := prepullProgress(ds, []corev1.Pod{mixed}); !st.done || st.pulled != 0 || st.resolved != 1 || len(st.failed) != 1 {
		t.Fatalf("mixed: %+v", st)
	}
}

func TestBuildPrepullDaemonSetCapsImages(t *testing.T) {
	var images []string
	for i := 0; i < maxPrepullImages+10; i++ {
		images = append(images, fmt.Sprintf("img-%d", i))
	}
	ds := buildPrepullDaemonSet("ns", "cls", images, nil, nil, nil)
	if len(ds.Spec.Template.Spec.Containers) != maxPrepullImages {
		t.Fatalf("containers = %d", len(ds.Spec.Template.Spec.Containers))
	}
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil || *ds.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("prepull pods need no service account token")
	}
	if ds.Labels[managedByLabel] != managedByValue {
		t.Fatal("managed-by label missing")
	}
}
