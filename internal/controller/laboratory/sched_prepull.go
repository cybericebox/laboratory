package laboratory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

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

// containerPullFailed reports whether the kubelet gave up on the container's image for
// now: it cannot be pulled (a wrong name or tag, no access) and nothing more will happen
// until it retries with back-off. Waiting for such an image would stall its group for
// nothing: the pods that use it fail through the normal image pull path.
func containerPullFailed(st *corev1.ContainerStatus) bool {
	if containerPulled(st) {
		return false
	}
	w := st.State.Waiting
	if w == nil {
		return false
	}
	switch w.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// prepullState is how far a prepull DaemonSet is.
type prepullState struct {
	// pulled counts the pods that hold all their images, resolved the pods that hold or
	// gave up on each of them, and desired the pods the DaemonSet wants.
	pulled, resolved, desired int
	// failed lists the images that could not be pulled, once each.
	failed []string
	// done is true once every scheduled pod has resolved all its images; a DaemonSet whose
	// status the controller has not reported yet is never done, and one with no eligible
	// node (desired 0) is done at once.
	done bool
}

// prepullProgress reads the progress of a prepull from its pods. An image that fails to
// pull counts as resolved at once (a pull error is reported within seconds), so one
// broken image does not hold the group until the timeout.
func prepullProgress(ds *appsv1.DaemonSet, pods []corev1.Pod) prepullState {
	var st prepullState
	if ds.Status.ObservedGeneration < ds.Generation || ds.Status.ObservedGeneration == 0 {
		return st
	}
	st.desired = int(ds.Status.DesiredNumberScheduled)
	seen := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || len(p.Status.ContainerStatuses) < len(p.Spec.Containers) {
			continue
		}
		pulledAll, resolvedAll := true, true
		for j := range p.Status.ContainerStatuses {
			cs := &p.Status.ContainerStatuses[j]
			switch {
			case containerPulled(cs):
			case containerPullFailed(cs):
				pulledAll = false
				image := cs.Image
				if image == "" && j < len(p.Spec.Containers) {
					image = p.Spec.Containers[j].Image
				}
				if !seen[image] {
					seen[image] = true
					st.failed = append(st.failed, image)
				}
			default:
				pulledAll, resolvedAll = false, false
			}
		}
		if pulledAll {
			st.pulled++
		}
		if resolvedAll {
			st.resolved++
		}
	}
	sort.Strings(st.failed)
	st.done = st.resolved >= st.desired
	return st
}
