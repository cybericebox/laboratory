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
	if _, _, done := prepullProgress(ds, nil); done {
		t.Fatal("status not observed yet: not done")
	}
	ds.Status.ObservedGeneration = 1
	if _, desired, done := prepullProgress(ds, nil); !done || desired != 0 {
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
	pulled, desired, done := prepullProgress(ds, []corev1.Pod{pod(true), pod(false)})
	if pulled != 1 || desired != 2 || done {
		t.Fatalf("pulled %d desired %d done %v", pulled, desired, done)
	}
	if _, _, done := prepullProgress(ds, []corev1.Pod{pod(true), pod(true)}); !done {
		t.Fatal("all pulled: done")
	}
	// A pod with no container status yet has pulled nothing.
	if pulled, _, _ := prepullProgress(ds, []corev1.Pod{{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "i0"}}}}}); pulled != 0 {
		t.Fatal("pod without statuses")
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
