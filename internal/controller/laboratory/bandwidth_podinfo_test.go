package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestMountOwnAnnotationsAddsTheVolumeAndTheMountOnce(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "gateway"}, {Name: "other"}}}
	mountOwnAnnotations(spec, "gateway")
	mountOwnAnnotations(spec, "gateway")
	if len(spec.Volumes) != 1 || spec.Volumes[0].DownwardAPI == nil || spec.Volumes[0].DownwardAPI.Items[0].FieldRef.FieldPath != "metadata.annotations" {
		t.Fatalf("volumes: %+v", spec.Volumes)
	}
	if len(spec.Containers[0].VolumeMounts) != 1 || spec.Containers[0].VolumeMounts[0].MountPath != "/etc/podinfo" || !spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatalf("mounts: %+v", spec.Containers[0].VolumeMounts)
	}
	if len(spec.Containers[1].VolumeMounts) != 0 {
		t.Fatal("only the named container gets the file")
	}
}
