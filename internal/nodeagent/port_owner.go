//go:build linux

package nodeagent

import (
	"context"
	"fmt"

	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// livePortUsers uses the uncached API. Pending replacements on this node need
// their stable keys too; waiting for Running would leave a takeover race.
func (r *NetworkAttachReconciler) livePortUsers(ctx context.Context, pod *corev1.Pod, key string) (map[types.UID]bool, error) {
	var pods corev1.PodList
	if err := r.directReader().List(ctx, &pods, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}
	users := map[types.UID]bool{}
	component := GroupComponent(pod)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.UID == pod.UID || p.Spec.NodeName != r.NodeName || !p.DeletionTimestamp.IsZero() || !isPlatformPod(p) {
			continue
		}
		if GroupComponent(p) != component {
			continue
		}
		if component != "" {
			attachments, err := GroupPodAttachments(ctx, r.directReader(), p.Namespace, component)
			if err != nil {
				return nil, err
			}
			for _, a := range attachments {
				if a.Name == key {
					users[p.UID] = true
				}
			}
		} else {
			for _, a := range ParseNetworkAnnotation(p.Annotations[names.AnnotationNetworks]) {
				if names.DevicePortKey(p.Namespace, p.Name, a.Iface) == key {
					users[p.UID] = true
				}
			}
		}
	}
	return users, nil
}

func (r *NetworkAttachReconciler) cleanupPodPort(ctx context.Context, pod *corev1.Pod, key string) error {
	owners, err := r.OVS.PortOwners()
	if err != nil {
		return err
	}
	uid, present := owners[key]
	if !present || uid != "" {
		return r.delVethWithFlowsOwned(key, pod.UID)
	}
	users, err := r.livePortUsers(ctx, pod, key)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: live replacement needs legacy port %s", ErrPortOwnerUnknown, key)
	}
	return r.OVS.delVethWithFlowsOwned(key, "", r.Flows)
}

// A replacement may retire a known old owner only after the direct API says
// that incarnation is terminating/absent. It may never seize a legacy row.
func (r *NetworkAttachReconciler) ensurePodPortOwner(ctx context.Context, pod *corev1.Pod, key string) error {
	owners, err := r.OVS.PortOwners()
	if err != nil {
		return err
	}
	uid, present := owners[key]
	if !present || uid == pod.UID {
		return nil
	}
	if uid == "" {
		return fmt.Errorf("%w: legacy port %s cannot be claimed", ErrPortOwnerUnknown, key)
	}
	var pods corev1.PodList
	if err := r.directReader().List(ctx, &pods, client.InNamespace(pod.Namespace)); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.UID == uid && p.DeletionTimestamp.IsZero() {
			return fmt.Errorf("%w: live owner %s", ErrPortOwnerChanged, uid)
		}
	}
	return r.delVethWithFlowsOwned(key, uid)
}
