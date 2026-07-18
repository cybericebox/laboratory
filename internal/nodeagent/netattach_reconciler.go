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
	"github.com/cybericebox/laboratory/internal/names"
)

const (
	AnnotationNetworks       = names.AnnotationNetworks
	AnnotationDefaultNetwork = names.AnnotationDefaultNetwork
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
		if name == "default" {
			continue
		}
		result = append(result, NetAttachment{Iface: iface, Name: name, MAC: mac})
	}
	return result
}

// NetworkAttachReconciler attaches veth ports to pods based on AnnotationNetworks.
// The host-side of each veth stays in root netns and is added to the OVS bridge;
// the pod-side is moved into the pod netns and renamed to the desired interface name.
type NetworkAttachReconciler struct {
	client.Client
	NodeName string
	OVS      *OVSManager
	Flows    *FlowManager
	CRISock  string
}

// delVethWithFlows removes the t0 entry for a veth port BEFORE deleting the
// port itself. OVS does not remove flows referencing a deleted port, and once
// the port is gone its name can no longer be resolved to an ofport — the stale
// flow would then match whichever interface OVS recycles that number to.
func (r *NetworkAttachReconciler) delVethWithFlows(stableKey string) {
	if r.Flows != nil {
		_ = r.Flows.DelT0Port(stableKey)
	}
	_ = r.OVS.DelVethPort(stableKey)
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
			stableKey := r.resolveOVSPort(ctx, pod.Namespace, devicePortOwner(&pod), att)
			r.delVethWithFlows(stableKey)
		}
		return ctrl.Result{}, nil
	}

	if len(attachments) == 0 {
		return ctrl.Result{}, nil
	}

	// Reconciler is a conformance check, not a wiring path. Ports present at pod
	// creation are wired synchronously by SetupNetworks during CNI ADD, which
	// completes before the pod is Running — so by the time we run, those ifaces
	// are already in the netns and the steady-state branch below is a no-op.
	// The reconciler only moves a veth for ports added to the annotation AFTER
	// the pod started (vpn/gateway lab interfaces), where SetupNetworks never re-runs.
	if pod.Status.Phase != corev1.PodRunning {
		log.Info("NetAttach: pod not running, requeueing")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	netnsPath, err := PodNetNSFromCRI(ctx, r.CRISock, string(pod.UID))
	if err != nil {
		log.Info("NetAttach: sandbox not ready, requeueing", "uid", string(pod.UID), "err", err)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	log.Info("NetAttach: got netnsPath", "netnsPath", netnsPath)

	for _, att := range attachments {
		stableKey := r.resolveOVSPort(ctx, pod.Namespace, devicePortOwner(&pod), att)
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
		log.Info(
			"NetAttach: attachment",
			"key",
			stableKey,
			"podSide",
			podSide,
			"targetIface",
			targetIface,
			"existsInOVS",
			exists,
		)
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
			// node-agent is L2 only: it moves the veth in and brings it up.
			// IP/MAC/route configuration is the device init-container's job.
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
						log.Error(
							err,
							"RenameInNetNS (recovery) failed, requeueing",
							"from",
							podSide,
							"to",
							targetIface,
						)
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
				r.delVethWithFlows(stableKey)
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
	}

	// No eth0 cleanup here. SetupNetworks blocks pod startup until ports are wired
	// and returns "stub" for pods with no default network, so cni-gate never creates
	// a real eth0 for them — only the required stub eth0. Deleting eth0 here would
	// remove that legitimate stub.

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// devicePortOwner returns the stable identity a device pod's OVS ports are keyed
// on: the Device CR name (LabelDeviceName), which is invariant across pod
// recreation under a Deployment and matches the key the Connection reconciler
// derives from the Device CR name. Falls back to the pod name for pods without
// the label (vpn/gateway and other non-device pods keep pod-scoped keys).
func devicePortOwner(pod *corev1.Pod) string {
	if dn := pod.Labels[names.LabelDeviceName]; dn != "" {
		return dn
	}
	return pod.Name
}

// resolveOVSPort returns the stable OVS port key for an attachment.
// Device-pod attachments ("iface@" or "iface@<connection>") always map to the
// deterministic DevicePortKey — the same key SetupNetworks creates the veth
// under. Names that do not resolve to a Connection CRD (vpn/gateway "labN@labN"
// entries) are literal OVS port names and are returned as-is. keyOwner is the
// port-key identity (see devicePortOwner), not necessarily the pod name.
//
// Never pick a port from Connection.Status.Ports here: a connection can have
// two local endpoints on this node, and any "first local port" heuristic wires
// one pod's attachment to the other pod's port.
func (r *NetworkAttachReconciler) resolveOVSPort(
	ctx context.Context,
	namespace, keyOwner string,
	att NetAttachment,
) string {
	if att.Name == "" {
		return names.DevicePortKey(namespace, keyOwner, att.Iface)
	}
	var conn laboratoryv1alpha1.Connection
	if err := r.Get(ctx, types.NamespacedName{Name: att.Name, Namespace: namespace}, &conn); err != nil {
		return att.Name
	}
	return names.DevicePortKey(namespace, keyOwner, att.Iface)
}

func (r *NetworkAttachReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
