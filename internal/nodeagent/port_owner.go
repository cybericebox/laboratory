//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"

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

// withVethLock runs fn holding the node's wiring lock and nothing else slow: the callers do their reads (API, CRI, planning) before
// it and only the mutation inside. who names the holder in the slow-lock log. A ctx that ends while waiting gives the lock up.
func (m *OVSManager) withVethLock(ctx context.Context, who string, fn func() error) error {
	if err := m.vethMu.lock(ctx, who); err != nil {
		return err
	}
	defer m.vethMu.Unlock()
	return fn()
}

// lockedDelVethOwned retires an owned port under the wiring lock. The owner row is checked against uid inside (ErrPortOwnerChanged
// when it is no longer the one that was planned), so this is also the re-check of the plan.
func (r *NetworkAttachReconciler) lockedDelVethOwned(ctx context.Context, who, key string, uid types.UID) error {
	return r.OVS.withVethLock(ctx, who, func() error { return r.delVethWithFlowsOwned(key, uid) })
}

// cacheReader is the informer-backed read: no API round trip, so it is safe to use while the wiring lock is held.
func (r *NetworkAttachReconciler) cacheReader() client.Reader {
	if r.Client != nil {
		return r.Client
	}
	return r.Reader
}

// podStillCurrent is the re-check made under the wiring lock: the pod that was planned for is still the same incarnation and not
// terminating. It reads the cache, never the API (the exact read was made before the lock was taken). A pod missing from the cache
// is gone.
func podStillCurrent(ctx context.Context, c client.Reader, pod *corev1.Pod) error {
	var cur corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &cur); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Errorf("%w: pod %s/%s is gone", ErrPortOwnerChanged, pod.Namespace, pod.Name)
		}
		return err
	}
	if cur.UID != pod.UID || !cur.DeletionTimestamp.IsZero() {
		return ErrPortOwnerChanged
	}
	return nil
}

// requireFreeOrOwned is the re-check of the port row made under the wiring lock: the key is free or already this incarnation's.
// Anything else (another incarnation took it, or a legacy row) is not ours to wire.
func (m *OVSManager) requireFreeOrOwned(key string, uid types.UID) error {
	owners, err := m.PortOwners()
	if err != nil {
		return err
	}
	if cur, present := owners[key]; present && cur != uid {
		return fmt.Errorf("%w: port %s changed owner before it was wired", ErrPortOwnerChanged, key)
	}
	return nil
}

func (r *NetworkAttachReconciler) cleanupPodPort(ctx context.Context, pod *corev1.Pod, key string) error {
	owners, err := r.OVS.PortOwners()
	if err != nil {
		return err
	}
	uid, present := owners[key]
	if !present && r.OVS.retired.retiredKey(key, pod.UID) {
		// This node-agent retired this Pod's port already (a Connection finalizer took it first) and the kernel link is
		// gone: nothing is left to clean. Anything else goes to the owned cleanup below, which refuses what it cannot prove.
		var absent netlink.LinkNotFoundError
		if _, err := netlink.LinkByName(key); errors.As(err, &absent) {
			return nil
		}
	}
	if present && uid != "" {
		if retiring, err := connectionRetiresPort(ctx, r.directReader(), pod.Namespace, r.NodeName, key, uid); err != nil || retiring {
			return err
		}
	}
	if !present || uid != "" {
		return r.lockedDelVethOwned(ctx, "cleanupPodPort", key, pod.UID)
	}
	users, err := r.livePortUsers(ctx, pod, key)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: live replacement needs legacy port %s", ErrPortOwnerUnknown, key)
	}
	return r.OVS.withVethLock(ctx, "cleanupPodPort", func() error {
		// The plan was "retire the legacy row"; it holds only while the row is still there and still without an owner.
		now, err := r.OVS.PortOwners()
		if err != nil {
			return err
		}
		if cur, ok := now[key]; !ok || cur != "" {
			return fmt.Errorf("%w: port %s changed before its legacy cleanup", ErrPortOwnerChanged, key)
		}
		return r.OVS.delVethWithFlowsOwned(key, "", r.Flows)
	})
}

// Neither this nor cleanupPodPort may be called with the wiring lock held: the reads run before it and the mutation takes it.
//
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
	return r.lockedDelVethOwned(ctx, "ensurePodPortOwner", key, uid)
}
