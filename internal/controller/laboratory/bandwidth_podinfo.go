package laboratory

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

const podinfoVolume = "podinfo"

// mountOwnAnnotations gives a container its pod's annotations as a file (a downward API volume at /etc/podinfo/annotations), where it
// reads the bandwidth ceiling the operator writes. The kubelet refreshes the file when the annotation changes, so the pod needs
// no access to the API for it. Only pods made from now on get it: a Deployment that exists keeps its template (a change would
// restart the gateway of a team that is running), and a pod without the file has no ceiling.
func mountOwnAnnotations(spec *corev1.PodSpec, container string) {
	for _, v := range spec.Volumes {
		if v.Name == podinfoVolume {
			return
		}
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: podinfoVolume, VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{
		Items: []corev1.DownwardAPIVolumeFile{{Path: "annotations", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations"}}},
	}}})
	for i := range spec.Containers {
		if spec.Containers[i].Name == container {
			spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, corev1.VolumeMount{
				Name: podinfoVolume, MountPath: bandwidth.AnnotationsDir, ReadOnly: true,
			})
		}
	}
}
