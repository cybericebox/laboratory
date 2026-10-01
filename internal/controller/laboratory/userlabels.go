package laboratory

import (
	"context"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"

	"github.com/cybericebox/laboratory/internal/names"
)

// User labels are the labels a user (or the platform) put on a Lab or LabGroup.
// They are copied onto the Devices of a Lab and onto the pods of Devices and
// LabGroups, so tooling can select pods by them. Labels the operator owns are
// never copied: the reserved prefix, and the keys the workloads select on.

// reservedLabelKeys are keys the operator or Kubernetes sets on pods itself.
var reservedLabelKeys = map[string]bool{
	"app": true, "pod-template-hash": true, "controller-revision-hash": true,
}

// userLabels returns the labels of an object that may be copied to its workloads.
func userLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if strings.HasPrefix(k, names.LabelPrefix) || reservedLabelKeys[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// applyUserLabels makes the user labels of obj equal to desired and reports
// whether obj changed. The keys it copied are recorded in an annotation, so a
// label that disappears from the source is removed here too, while the labels
// the operator itself put on obj are left alone.
func applyUserLabels(obj metav1.Object, desired map[string]string) bool {
	labels := obj.GetLabels()
	annotations := obj.GetAnnotations()
	changed := false
	if prev := annotations[names.AnnotationUserLabels]; prev != "" {
		for _, k := range strings.Split(prev, ",") {
			if _, keep := desired[k]; keep {
				continue
			}
			if _, has := labels[k]; has {
				delete(labels, k)
				changed = true
			}
		}
	}
	for k, v := range desired {
		if labels == nil {
			labels = map[string]string{}
		}
		if labels[k] != v {
			labels[k] = v
			changed = true
		}
	}
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := strings.Join(keys, ",")
	if annotations[names.AnnotationUserLabels] != want {
		if annotations == nil {
			annotations = map[string]string{}
		}
		if want == "" {
			delete(annotations, names.AnnotationUserLabels)
		} else {
			annotations[names.AnnotationUserLabels] = want
		}
		obj.SetAnnotations(annotations)
		changed = true
	}
	if labels != nil || obj.GetLabels() != nil {
		obj.SetLabels(labels)
	}
	return changed
}

// joinKeys is the sorted, comma separated keys of a label set.
func joinKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// syncPodLabels keeps the user labels of the device's live pods equal to the
// device's. Only the pod metadata changes, never the pod template, so a label
// change restarts nothing; a pod created later from the template carries the
// labels of its creation time and is brought up to date here.
func (r *DeviceReconciler) syncPodLabels(ctx context.Context, device *laboratoryv1alpha1.Device) error {
	desired := userLabels(device.Labels)
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(device.Namespace), client.MatchingLabels{
		names.LabelLab: device.Spec.LabRef, names.LabelDevice: device.Spec.Name,
	}); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		orig := p.DeepCopy()
		if applyUserLabels(p, desired) {
			if err := r.Patch(ctx, p, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}
