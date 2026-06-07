//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/ovsnames"
)

// AnnotationNetworks and DefaultNetworkValue are defined in api/laboratory/v1alpha1/shared_types.go.
// Re-exported here as package-level aliases for convenience.
const (
	AnnotationNetworks = laboratoryv1alpha1.AnnotationNetworks
	DefaultNetworkName = laboratoryv1alpha1.DefaultNetworkValue
)

// NetAttachment is one parsed entry from the networks annotation.
type NetAttachment struct {
	Iface string // desired name inside pod netns (e.g. "eth1")
	Name  string // Connection CRD name or OVS port name
	MAC   string // optional hardware address; empty = keep generated MAC
}

// ParseNetworkAnnotation parses the network.cybericebox.com/networks annotation.
// Format per entry: "iface@[connection][|MAC]"
// Entries with name=="default" are excluded (handled by the CNI plugin).
func ParseNetworkAnnotation(annotation string) []NetAttachment {
	var result []NetAttachment
	for _, entry := range strings.Split(annotation, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		iface, rest, ok := strings.Cut(entry, "@")
		if !ok || iface == "" {
			continue
		}
		name, mac, _ := strings.Cut(rest, "|")
		if name == DefaultNetworkName {
			continue
		}
		result = append(result, NetAttachment{Iface: iface, Name: name, MAC: mac})
	}
	return result
}

// HasDefaultEth0 reports whether the annotation requests the real Kubernetes eth0.
func HasDefaultEth0(annotation string) bool {
	for _, entry := range strings.Split(annotation, ",") {
		iface, name, ok := strings.Cut(strings.TrimSpace(entry), "@")
		if ok && iface == "eth0" && name == DefaultNetworkName {
			return true
		}
	}
	return false
}

// NetworkAttachReconciler attaches veth ports to pods based on AnnotationNetworks.
// The host-side of each veth stays in root netns and is added to the OVS bridge;
// the pod-side is moved into the pod netns and renamed to the desired interface name.
type NetworkAttachReconciler struct {
	client.Client
	NodeName string
	OVS      *OVSManager
	Server   *NodeAgentServer
	ProcRoot string
}

func (r *NetworkAttachReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("NetAttach reconcile start", "pod", req.NamespacedName)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if pod.Spec.NodeName != r.NodeName {
		log.Info("NetAttach: skip, wrong node", "podNode", pod.Spec.NodeName, "myNode", r.NodeName)
		return ctrl.Result{}, nil
	}

	annotation := pod.Annotations[AnnotationNetworks]
	attachments := ParseNetworkAnnotation(annotation)
	log.Info("NetAttach parsed", "annotation", annotation, "attachments", len(attachments), "phase", pod.Status.Phase)

	if pod.DeletionTimestamp != nil {
		for _, att := range attachments {
			stableKey := r.resolveOVSPort(ctx, pod.Namespace, pod.Name, att)
			_ = r.OVS.DelVethPort(stableKey)
		}
		return ctrl.Result{}, nil
	}

	if len(attachments) == 0 {
		return ctrl.Result{}, nil
	}

	if pod.Status.Phase != corev1.PodRunning {
		log.Info("NetAttach: pod not running, requeueing")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	netnsPath, ok := r.Server.GetPodNetNS(string(pod.UID))
	if !ok {
		var err error
		netnsPath, err = FindPodNetNS(r.ProcRoot, string(pod.UID))
		if err != nil {
			log.Error(err, "NetAttach: FindPodNetNS failed, requeueing", "procRoot", r.ProcRoot, "uid", string(pod.UID))
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	log.Info("NetAttach: got netnsPath", "netnsPath", netnsPath)

	for _, att := range attachments {
		stableKey := r.resolveOVSPort(ctx, pod.Namespace, pod.Name, att)
		podSide := VethPeerName(stableKey)
		targetIface := att.Iface
		if targetIface == "" {
			targetIface = stableKey
		}

		// Ensure the veth host-side is registered in OVS.
		_, exists, err := r.OVS.FindPortByKey(stableKey)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("find veth port %q: %w", stableKey, err)
		}
		log.Info("NetAttach: attachment", "key", stableKey, "podSide", podSide, "targetIface", targetIface, "existsInOVS", exists)
		if !exists {
			if err := r.OVS.AddVethPort(stableKey); err != nil {
				return ctrl.Result{}, fmt.Errorf("add veth port %q: %w", stableKey, err)
			}
			log.Info("NetAttach: created veth pair", "hostSide", stableKey, "podSide", podSide)
		}

		// Check whether pod-side veth is still in root netns.
		podSideInRoot := WaitForLink(podSide, 200*time.Millisecond) == nil
		log.Info("NetAttach: pod-side location", "podSide", podSide, "inRootNetns", podSideInRoot)

		if podSideInRoot {
			// Pod-side in root netns — clear any stale targetIface in pod netns, then move.
			if CheckInNetNS(netnsPath, targetIface) == nil {
				log.Info("NetAttach: stale targetIface in pod netns, removing", "iface", targetIface)
				if delErr := DeleteInNetNS(netnsPath, targetIface); delErr != nil {
					// Cannot delete (e.g. OVS orphan) — rename it out of the way.
					log.Error(delErr, "DeleteInNetNS failed, renaming stale iface", "iface", targetIface)
					tmpName := "x" + targetIface
					if renErr := RenameInNetNS(netnsPath, targetIface, tmpName); renErr != nil {
						log.Error(renErr, "rename stale iface failed, requeueing", "iface", targetIface)
						return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
					}
				}
			}
			log.Info("NetAttach: moving pod-side to netns", "podSide", podSide, "netns", netnsPath)
			if err := MoveToNetNS(podSide, netnsPath); err != nil {
				log.Error(err, "MoveToNetNS failed, requeueing", "podSide", podSide)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			log.Info("NetAttach: MoveToNetNS OK", "podSide", podSide)
			if podSide != targetIface {
				if err := RenameInNetNS(netnsPath, podSide, targetIface); err != nil {
					log.Error(err, "RenameInNetNS failed, requeueing", "from", podSide, "to", targetIface)
					return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
				}
				log.Info("NetAttach: RenameInNetNS OK", "from", podSide, "to", targetIface)
			}
			if att.MAC != "" {
				if err := SetMACInNetNS(netnsPath, targetIface, att.MAC); err != nil {
					log.Error(err, "SetMACInNetNS failed, requeueing", "iface", targetIface)
					return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
				}
			}
			if err := BringUpInNetNS(netnsPath, targetIface); err != nil {
				log.Error(err, "BringUpInNetNS failed, requeueing", "iface", targetIface)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			if err := ConfigureInNetNS(netnsPath, targetIface, ""); err != nil {
				log.Error(err, "ConfigureInNetNS failed, requeueing", "iface", targetIface)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			log.Info("NetAttach: veth setup complete", "iface", targetIface)
		} else {
			// Pod-side not in root netns — already moved or stale.
			underTarget := CheckInNetNS(netnsPath, targetIface)
			if underTarget == nil {
				// Already in pod netns under correct name — ensure UP.
				if err := BringUpInNetNS(netnsPath, targetIface); err != nil {
					log.Error(err, "BringUpInNetNS (steady-state) failed, requeueing", "iface", targetIface)
					return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
				}
				log.Info("NetAttach: already in pod netns, ensured UP", "iface", targetIface)
			} else if CheckInNetNS(netnsPath, podSide) == nil {
				// Partial: moved to pod netns but not yet renamed.
				if podSide != targetIface {
					if err := RenameInNetNS(netnsPath, podSide, targetIface); err != nil {
						log.Error(err, "RenameInNetNS (recovery) failed, requeueing", "from", podSide, "to", targetIface)
						return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
					}
					log.Info("NetAttach: recovery rename OK", "from", podSide, "to", targetIface)
				}
				if err := BringUpInNetNS(netnsPath, targetIface); err != nil {
					log.Error(err, "BringUpInNetNS (recovery) failed, requeueing", "iface", targetIface)
					return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
				}
			} else {
				// Not in root netns, not in pod netns — stale veth, recreate.
				log.Info("NetAttach: stale veth, recreating", "key", stableKey)
				_ = r.OVS.DelVethPort(stableKey)
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// resolveOVSPort returns the stable OVS port key for an attachment.
func (r *NetworkAttachReconciler) resolveOVSPort(ctx context.Context, namespace, podName string, att NetAttachment) string {
	if att.Name == "" {
		return ovsnames.DevicePortKey(namespace, podName, att.Iface)
	}
	var conn laboratoryv1alpha1.Connection
	if err := r.Get(ctx, types.NamespacedName{Name: att.Name, Namespace: namespace}, &conn); err != nil {
		return att.Name
	}
	for _, p := range conn.Status.Ports {
		if p.NodeName == r.NodeName && p.PortID != "" {
			return p.PortID
		}
	}
	return att.Name
}

func (r *NetworkAttachReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
