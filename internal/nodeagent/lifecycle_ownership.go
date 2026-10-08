//go:build linux

package nodeagent

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Native ownership follows immutable controller UIDs, never matching labels or
// names alone. Both observation and prephysical capture use this same chain.
func nativePodDeviceLab(ctx context.Context, reader client.Reader, pod *corev1.Pod) (*lab.Device, *lab.Lab, error) {
	refs := pod.OwnerReferences
	for _, ref := range refs {
		deviceRef := ref
		if ref.Kind == "ReplicaSet" {
			var rs appsv1.ReplicaSet
			if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, &rs); err != nil {
				return nil, nil, err
			}
			if rs.UID != ref.UID {
				return nil, nil, ErrPortOwnerChanged
			}
			deployment, ok := nativeOwnerReference(rs.OwnerReferences, "Deployment")
			if !ok {
				return nil, nil, ErrPortOwnerUnknown
			}
			var dep appsv1.Deployment
			if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: deployment.Name}, &dep); err != nil {
				return nil, nil, err
			}
			if dep.UID != deployment.UID {
				return nil, nil, ErrPortOwnerChanged
			}
			var found bool
			deviceRef, found = nativeOwnerReference(dep.OwnerReferences, "Device")
			if !found {
				return nil, nil, ErrPortOwnerUnknown
			}
		} else if ref.Kind != "Device" {
			continue
		}
		var device lab.Device
		if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: deviceRef.Name}, &device); err != nil {
			return nil, nil, err
		}
		if device.UID != deviceRef.UID {
			return nil, nil, ErrPortOwnerChanged
		}
		var parent lab.Lab
		if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: device.Spec.LabRef}, &parent); err != nil {
			return nil, nil, err
		}
		owner, ok := nativeOwnerReference(device.OwnerReferences, "Lab")
		if !ok || owner.UID != parent.UID {
			return nil, nil, ErrPortOwnerChanged
		}
		return &device, &parent, nil
	}
	return nil, nil, ErrPortOwnerUnknown
}
func nativeOwnerReference(refs []metav1.OwnerReference, kind string) (metav1.OwnerReference, bool) {
	for _, ref := range refs {
		if ref.Kind == kind && ref.UID != "" {
			return ref, true
		}
	}
	return metav1.OwnerReference{}, false
}
