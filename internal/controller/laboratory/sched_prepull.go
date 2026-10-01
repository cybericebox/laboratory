package laboratory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels of the short-lived DaemonSet that pulls the images of a launch class.
const (
	prepullLabel      = "laboratory.cybericebox.com/prepull"
	prepullClassLabel = "laboratory.cybericebox.com/prepull-class"
	managedByLabel    = "app.kubernetes.io/managed-by"
	managedByValue    = "laboratory-operator"

	// maxPrepullImages bounds the containers of one prepull pod. A class has a
	// handful of images; a larger set is cut, and the rest is pulled on demand.
	maxPrepullImages = 40
)

// prepullKey is the stable short name suffix of a class: class ids are free text
// and not valid label values.
func prepullKey(class string) string {
	sum := sha256.Sum256([]byte(class))
	return hex.EncodeToString(sum[:])[:10]
}

func prepullName(class string) string { return "prepull-" + prepullKey(class) }

// buildPrepullDaemonSet builds the DaemonSet that puts every image of a class on
// every eligible node. It runs one container per image, each with its command
// replaced by a sleep, so the lab service itself never starts: the kubelet pulls
// the image and reports its ImageID, which is all the launcher waits for. Pods
// use the operator's pull secrets and the lab node selector and tolerations.
func buildPrepullDaemonSet(namespace, class string, images []string, pullSecrets []string,
	nodeSelector map[string]string, tolerations []corev1.Toleration) *appsv1.DaemonSet {
	if len(images) > maxPrepullImages {
		images = images[:maxPrepullImages]
	}
	key := prepullKey(class)
	labels := map[string]string{
		managedByLabel: managedByValue,
		prepullLabel:   key,
	}
	containers := make([]corev1.Container, 0, len(images))
	for i, img := range images {
		containers = append(containers, corev1.Container{
			Name:            fmt.Sprintf("i%d", i),
			Image:           img,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", "exec sleep 3600"},
		})
	}
	noToken := false
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        prepullName(class),
			Namespace:   namespace,
			Labels:      labels,
			Annotations: map[string]string{prepullClassLabel: class},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{prepullLabel: key}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  &noToken,
					TerminationGracePeriodSeconds: new(int64),
					ImagePullSecrets:              pullSecretRefs(pullSecrets),
					NodeSelector:                  nodeSelector,
					Tolerations:                   tolerations,
					Containers:                    containers,
				},
			},
		},
	}
}

// containerPulled reports whether the kubelet has the container's image: it
// recorded an image ID, or the container already started or ran.
func containerPulled(st *corev1.ContainerStatus) bool {
	return st.ImageID != "" || st.State.Running != nil || st.State.Terminated != nil
}

// prepullProgress counts how many pods hold all their images and how many pods
// the DaemonSet wants. done is true once every scheduled pod has all images; a
// DaemonSet whose status the controller has not reported yet is never done, and
// one with no eligible node (desired 0) is done at once.
func prepullProgress(ds *appsv1.DaemonSet, pods []corev1.Pod) (pulled, desired int, done bool) {
	if ds.Status.ObservedGeneration < ds.Generation || ds.Status.ObservedGeneration == 0 {
		return 0, 0, false
	}
	desired = int(ds.Status.DesiredNumberScheduled)
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || len(p.Status.ContainerStatuses) < len(p.Spec.Containers) {
			continue
		}
		all := true
		for j := range p.Status.ContainerStatuses {
			if !containerPulled(&p.Status.ContainerStatuses[j]) {
				all = false
				break
			}
		}
		if all {
			pulled++
		}
	}
	return pulled, desired, pulled >= desired
}
