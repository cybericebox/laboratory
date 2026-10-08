//go:build linux

package nodeagent

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Resource versions and managed fields do not change an attachment. Ownership,
// identity and deletion still matter even when the object's generation is equal.
func networkMetadataChanged(a, b client.Object) bool {
	return a.GetUID() != b.GetUID() || a.GetName() != b.GetName() || a.GetNamespace() != b.GetNamespace() ||
		a.GetGeneration() != b.GetGeneration() ||
		!reflect.DeepEqual(a.GetLabels(), b.GetLabels()) ||
		!reflect.DeepEqual(a.GetAnnotations(), b.GetAnnotations()) ||
		!reflect.DeepEqual(a.GetOwnerReferences(), b.GetOwnerReferences()) ||
		!reflect.DeepEqual(a.GetFinalizers(), b.GetFinalizers()) ||
		!reflect.DeepEqual(a.GetDeletionTimestamp(), b.GetDeletionTimestamp()) ||
		!reflect.DeepEqual(a.GetDeletionGracePeriodSeconds(), b.GetDeletionGracePeriodSeconds())
}

// Snapshot progress and readiness reporting do not change connection endpoint
// placement. Explicit 60s recovery requeues continue independently of this watch.
func connectionDeviceInputs() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, okA := e.ObjectOld.(*api.Device)
		b, okB := e.ObjectNew.(*api.Device)
		if !okA || !okB {
			return true
		}
		return networkMetadataChanged(a, b) || !reflect.DeepEqual(a.Spec, b.Spec) ||
			a.Status.NodeName != b.Status.NodeName || a.Status.NodeAddress != b.Status.NodeAddress ||
			a.Status.PodName != b.Status.PodName || !reflect.DeepEqual(a.Status.VNI, b.Status.VNI)
	}}
}

func networkPodInputs(node string) predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, okA := e.ObjectOld.(*corev1.Pod)
		b, okB := e.ObjectNew.(*corev1.Pod)
		if !okA || !okB {
			return true
		}
		local := func(p *corev1.Pod) bool { return p.Spec.NodeName == node && isPlatformPod(p) }
		if !local(a) && !local(b) {
			return false
		}
		return networkMetadataChanged(a, b) || !reflect.DeepEqual(a.Spec, b.Spec) || !reflect.DeepEqual(a.Status, b.Status)
	}}
}
