//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/netattach"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

const (
	AnnotationNetworks       = names.AnnotationNetworks
	AnnotationDefaultNetwork = names.AnnotationDefaultNetwork
)

// NetAttachment is one parsed entry from the networks annotation.
type NetAttachment = netattach.Attachment

// ParseNetworkAnnotation parses the network.cybericebox.com/networks annotation (see
// netattach.Parse). Entries whose interface name or MAC is not valid are dropped: the
// operator and the API validate both, and a value that got past them must not rename
// or re-address anything in the pod netns.
func ParseNetworkAnnotation(annotation string) []NetAttachment {
	var result []NetAttachment
	for _, att := range netattach.Parse(annotation) {
		if netattach.ValidateInterfaceName(att.Iface) != nil {
			continue
		}
		if netattach.ValidateMAC(att.MAC) != nil {
			continue
		}
		result = append(result, att)
	}
	return result
}

// NetworkAttachReconciler attaches veth ports to pods based on AnnotationNetworks.
// The host-side of each veth stays in root netns and is added to the OVS bridge;
// the pod-side is moved into the pod netns and renamed to the desired interface name.
type NetworkAttachReconciler struct {
	client.Client
	Reader   client.Reader
	NodeName string
	OVS      *OVSManager
	Flows    *FlowManager
	CRISock  string

	guardMu sync.Mutex
	guard   *recreationGuard
}

// mayRecreate says whether the veth of a pod may be recreated again (see recreationGuard); when not, how long to leave it be.
func (r *NetworkAttachReconciler) mayRecreate(key string) (bool, time.Duration) {
	r.guardMu.Lock()
	defer r.guardMu.Unlock()
	if r.guard == nil {
		r.guard = newRecreationGuard(5, 10*time.Minute)
	}
	return r.guard.allow(key, time.Now())
}

// DelVethWithFlowsOwned returns an error rather than a cleanup ACK when the
// stable key belongs to another incarnation or its ownership is unknown.
func (r *NetworkAttachReconciler) DelVethWithFlowsOwned(key string, ownerUID types.UID) error {
	if ownerUID == "" || r.Flows == nil {
		return ErrPortOwnerUnknown
	}
	r.OVS.vethMu.Lock()
	defer r.OVS.vethMu.Unlock()
	return r.delVethWithFlowsOwned(key, ownerUID)
}

// The reconciler and CNI already hold the Pod wiring lock.
func (r *NetworkAttachReconciler) delVethWithFlowsOwned(key string, ownerUID types.UID) error {
	if ownerUID == "" {
		return ErrPortOwnerUnknown
	}
	return r.OVS.delVethWithFlowsOwned(key, ownerUID, r.Flows)
}

// isPlatformPod says whether a pod is one the platform made for a lab: its labels carry the lab and the device, or the component
// (vpn, gateway) of a group. The operator sets them and no caller can.
func isPlatformPod(pod *corev1.Pod) bool {
	if _, ok := pod.Labels[names.LabelLab]; ok {
		_, dev := pod.Labels[names.LabelDevice]
		return dev
	}
	c := pod.Labels[names.LabelComponent]
	if c == names.ComponentVPN || c == names.ComponentGateway {
		return true
	}
	// A VPN or gateway pod made before the component label: by `app`.
	app := pod.Labels["app"]
	return app == names.ComponentVPN || app == names.ComponentGateway
}

//nolint:gocyclo // one decision over many cases; splitting it would scatter the rule
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
	// Only the platform's own pods are wired: a device pod or the VPN or gateway of a group. Any other pod on the node
	// that carries the annotation (a workload of something else) is not ours to touch.
	if !isPlatformPod(&pod) {
		return ctrl.Result{}, nil
	}

	// Different Pod names share group stable keys. Hold the node's wiring
	// lock through direct identity checks and all netns mutations.
	r.OVS.vethMu.Lock()
	defer r.OVS.vethMu.Unlock()

	// The VPN and gateway pod of a group: the lab interfaces come from the group's LabVPN and LabGateway objects (see GroupPodAttachments),
	// not from the pod's annotation, so a lab added or removed never changes the Deployment. A device pod lists its interfaces in the
	// annotation.
	component := GroupComponent(&pod)
	var attachments []NetAttachment
	if component != "" {
		var err error
		if attachments, err = GroupPodAttachments(ctx, r.directReader(), pod.Namespace, component); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		attachments = ParseNetworkAnnotation(pod.Annotations[AnnotationNetworks])
	}
	log.Info("NetAttach attachments", "component", component, "attachments", len(attachments), "phase", pod.Status.Phase)

	stopped := false
	if component == "" {
		active, err := labRuntimeActive(ctx, r.directReader(), pod.Namespace, pod.Labels[names.LabelLab])
		if err != nil {
			return ctrl.Result{}, err
		}
		stopped = !active
	}
	if pod.DeletionTimestamp != nil || stopped {
		if component != "" {
			owners, err := r.OVS.PortOwners()
			if err != nil {
				return ctrl.Result{}, err
			}
			for _, key := range GroupPortsPresentOwned(pod.Namespace, component, pod.UID, owners) {
				if err := r.delVethWithFlowsOwned(key, pod.UID); err != nil {
					return ctrl.Result{}, err
				}
			}
			// Legacy rows carry no incarnation. Only a direct live replacement
			// check can authorize retiring them; uncertainty is not an ACK.
			legacy := map[string]bool{}
			for key, uid := range owners {
				if uid == "" {
					legacy[key] = true
				}
			}
			for _, key := range GroupPortsPresent(pod.Namespace, component, legacy) {
				if err := r.cleanupPodPort(ctx, &pod, key); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
		for _, att := range attachments {
			stableKey := r.resolveOVSPort(ctx, pod.Namespace, pod.Name, att)
			if !ValidPortKey(stableKey) {
				log.Info("NetAttach: not a port key of the platform, ignored", "name", stableKey)
				continue
			}
			if err := r.cleanupPodPort(ctx, &pod, stableKey); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// A lab that is gone: its leg is taken out of the running pod (the pod keeps running).
	if component != "" && pod.Status.Phase == corev1.PodRunning {
		owners, err := r.OVS.PortOwners()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("list ports: %w", err)
		}
		present := map[string]bool{}
		for key, uid := range owners {
			if uid == pod.UID || uid == "" {
				present[key] = true
			}
		}
		for _, key := range StaleGroupPorts(pod.Namespace, component, attachments, present) {
			log.Info("NetAttach: detaching the leg of a lab that is gone", "key", key)
			if err := r.cleanupPodPort(ctx, &pod, key); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if len(attachments) == 0 {
		if component != "" {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
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

	var current corev1.Pod
	if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(&pod), &current); err != nil {
		return ctrl.Result{}, err
	}
	if current.UID != pod.UID || !current.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, ErrPortOwnerChanged
	}
	netnsPath, err := PodNetNSFromCRI(ctx, r.CRISock, string(pod.UID))
	if err != nil {
		log.Info("NetAttach: sandbox not ready, requeueing", "uid", string(pod.UID), "err", err)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	log.Info("NetAttach: got netnsPath", "netnsPath", netnsPath)

	recreated := false
	for _, att := range attachments {
		stableKey := att.Name // a leg of a lab: its port is named by the lab's index
		if component == "" {
			stableKey = r.resolveOVSPort(ctx, pod.Namespace, pod.Name, att)
		}
		if !ValidPortKey(stableKey) {
			// "eth0" or any other name an annotation could carry: never a veth of ours.
			log.Info("NetAttach: not a port key of the platform, ignored", "name", stableKey)
			continue
		}
		podSide := VethPeerName(stableKey)
		targetIface := att.Iface
		if targetIface == "" {
			targetIface = stableKey
		}

		if err := r.ensurePodPortOwner(ctx, &pod, stableKey); err != nil {
			return ctrl.Result{}, err
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
		if exists {
			// A port made before the policing setting (or under another value) gets it now; a no-op when it already has it.
			if err := r.OVS.EnsurePolicing(stableKey); err != nil {
				log.Error(err, "NetAttach: police veth port", "key", stableKey)
			}
		}
		if !exists {
			if err := r.OVS.AddVethPortOwned(stableKey, pod.UID); err != nil {
				return ctrl.Result{}, fmt.Errorf("add veth port %q: %w", stableKey, err)
			}
			log.Info("NetAttach: created veth pair", "hostSide", stableKey, "podSide", podSide)
		}

		// Check whether pod-side veth is still in root netns.
		podSideInRoot := peerInRoot(podSide, !exists)
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
				// Not in root netns, not in pod netns — stale veth, recreate. A pod that makes this happen again and again (it can delete its
				// own interface when it has NET_ADMIN) is left alone for a while instead.
				if ok, wait := r.mayRecreate(string(pod.UID) + "/" + stableKey); !ok {
					log.Info("NetAttach: the veth of this pod was recreated too often, waiting", "key", stableKey, "wait", wait.String())
					// Restore peers already retired in this pass before entering
					// this peer's cooldown; the capped peer remains untouched.
					if recreated {
						return ctrl.Result{RequeueAfter: time.Second}, nil
					}
					return ctrl.Result{RequeueAfter: wait}, nil
				}
				log.Info("NetAttach: stale veth, recreating", "key", stableKey)
				if err := r.cleanupPodPort(ctx, &pod, stableKey); err != nil {
					return ctrl.Result{}, err
				}
				recreated = true
			}
		}
	}

	// No eth0 cleanup here. SetupNetworks blocks pod startup until ports are wired
	// and returns "stub" for pods with no default network, so cni-gate never creates
	// a real eth0 for them — only the required stub eth0. Deleting eth0 here would
	// remove that legitimate stub.

	// A replacement pod loses every peer together. Retire the entire stale
	// batch before the one recovery requeue, instead of waiting a second per
	// interface and rescanning all already-restored interfaces on every pass.
	if recreated {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// resolveOVSPort returns the stable OVS port key for an attachment.
// Device-pod attachments ("iface@" or "iface@<connection>") always map to the
// per-pod DevicePortKey — the same key SetupNetworks creates the veth under, so
// each pod owns its own port and a recreated pod gets a fresh one (no stale-port
// overlap with the terminating pod). Names that do not resolve to a Connection
// CRD (vpn/gateway "labN@labN" entries) are literal OVS port names, returned
// as-is. The Connection reconciler binds whichever pod is current (Device
// Status.PodName) into the VNI, so per-pod keys need no coordination here.
//
// Never pick a port from Connection.Status.Ports here: a connection can have
// two local endpoints on this node, and any "first local port" heuristic wires
// one pod's attachment to the other pod's port.
func (r *NetworkAttachReconciler) resolveOVSPort(
	ctx context.Context,
	namespace, podName string,
	att NetAttachment,
) string {
	if att.Name == "" {
		return names.DevicePortKey(namespace, podName, att.Iface)
	}
	var conn laboratoryv1alpha1.Connection
	if err := r.Get(ctx, types.NamespacedName{Name: att.Name, Namespace: namespace}, &conn); err != nil {
		return att.Name
	}
	return names.DevicePortKey(namespace, podName, att.Iface)
}

// groupPodsOf enqueues the VPN or gateway pod of the group (on this node) when one of its lab network objects changes.
func (r *NetworkAttachReconciler) groupPodsOf(component string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Spec.NodeName == r.NodeName && GroupComponent(p) == component {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
			}
		}
		return out
	}
}

func (r *NetworkAttachReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Reader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(networkPodInputs(r.NodeName))).
		Watches(&laboratoryv1alpha1.Lab{}, handler.EnqueueRequestsFromMapFunc(r.podsForLab)).
		Watches(&laboratoryv1alpha1.LabVPN{}, handler.EnqueueRequestsFromMapFunc(r.groupPodsOf(names.ComponentVPN))).
		Watches(&laboratoryv1alpha1.LabGateway{}, handler.EnqueueRequestsFromMapFunc(r.groupPodsOf(names.ComponentGateway))).
		Complete(reconcileutil.QuietIgnoreNotFound(r))
}

func (r *NetworkAttachReconciler) podsForLab(ctx context.Context, obj client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, p := range pods.Items {
		if p.Spec.NodeName == r.NodeName && (GroupComponent(&p) != "" || p.Labels[names.LabelLab] == obj.GetName()) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
		}
	}
	return out
}

func (r *NetworkAttachReconciler) directReader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// DelVethWithFlowsOwnedJournaled fsyncs the physical cleanup witness while the
// exact owner row still exists. Only its guarded row retirement follows it.
func (r *NetworkAttachReconciler) DelVethWithFlowsOwnedJournaled(key string, uid types.UID, prepared func(string) error) error {
	if uid == "" || r.Flows == nil || prepared == nil {
		return ErrPortOwnerUnknown
	}
	r.OVS.vethMu.Lock()
	defer r.OVS.vethMu.Unlock()
	return r.OVS.delVethWithFlowsOwnedJournaled(key, uid, r.Flows, prepared)
}

func (r *NetworkAttachReconciler) DelVethWithFlowsExpected(key string, uid types.UID, row string) error {
	if row == "" {
		return ErrPortOwnerUnknown
	}
	return r.OVS.delVethWithFlowsOwnedJournaled(key, uid, r.Flows, nil, row)
}
