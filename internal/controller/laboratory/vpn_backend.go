package laboratory

import (
	"context"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// vpnBackend is the address the demux sends the handshakes of the group to: "<pod IP>:<port>" of the group's ready VPN pod, written to
// LabGroup.status.vpn.backend on every reconcile (a change of the VPN pod re-reconciles the group, SetupWithManager). The VPN Service is
// headless on purpose (no ClusterIP NAT in the WireGuard path) and the demux uses no cluster DNS, so the pod address is what it gets.
//
// Empty means "no address to give": the deployment has no ready replica, or no VPN pod is Running and Ready with an address yet (a pod that is
// starting, terminating or replaced). The demux drops the group's handshakes until a fresh address arrives, it never keeps the old one.
// With two ready pods (a rollout) the newest one is the backend.
func (r *LabGroupReconciler) vpnBackend(ctx context.Context, ns string, deploymentReady bool) (string, error) {
	if !deploymentReady {
		return "", nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return "", err
	}
	var newest *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if !isVPNPod(p) || !vpnPodServing(p) {
			continue
		}
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	if newest == nil {
		return "", nil
	}
	return net.JoinHostPort(newest.Status.PodIP, strconv.Itoa(int(r.vpnPort()))), nil
}

// vpnPodServing says the pod can take the demux's packets now: not deleting, Running, Ready, with a unicast address.
func vpnPodServing(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	ip := net.ParseIP(p.Status.PodIP)
	return ready && ip != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}
