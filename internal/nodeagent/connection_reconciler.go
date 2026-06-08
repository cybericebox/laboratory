//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// ConnectionReconciler programs br-ovs based on Connection CRDs.
type ConnectionReconciler struct {
	client.Client
	NodeName    string
	NodeAddress string
	OVS         *OVSManager
	Flows       *FlowManager
	ProcRoot    string
}

func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var conn laboratoryv1alpha1.Connection
	if err := r.Get(ctx, req.NamespacedName, &conn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !conn.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &conn)
	}

	return r.reconcileCreate(ctx, &conn)
}

func (r *ConnectionReconciler) reconcileCreate(ctx context.Context, conn *laboratoryv1alpha1.Connection) (ctrl.Result, error) {
	type epInfo struct {
		endpoint  laboratoryv1alpha1.EndpointSpec
		device    laboratoryv1alpha1.Device
		isSwitch  bool
		nodeReady bool
	}

	eps := make([]epInfo, 0, len(conn.Spec.Endpoints))
	needsRequeue := false

	for _, ep := range conn.Spec.Endpoints {
		// Virtual singletons (vpn, internet) have no Device CRD — synthesize from their pods.
		if ep.Device == "vpn" || ep.Device == "internet" {
			appLabel := ep.Device
			var pods corev1.PodList
			if err := r.List(ctx, &pods,
				client.InNamespace(conn.Namespace),
				client.MatchingLabels{"app": appLabel},
				client.Limit(1),
			); err != nil {
				return ctrl.Result{}, err
			}
			if len(pods.Items) == 0 || pods.Items[0].Spec.NodeName == "" {
				needsRequeue = true
				// Synthesize empty device so the slice length stays consistent.
				eps = append(eps, epInfo{ep, laboratoryv1alpha1.Device{}, false, false})
				continue
			}
			pod := pods.Items[0]
			synth := laboratoryv1alpha1.Device{}
			synth.Spec.Type = laboratoryv1alpha1.DeviceTypeContainer
			synth.Status.NodeName = pod.Spec.NodeName
			synth.Status.NodeAddress = r.nodeAddressForNode(pod.Spec.NodeName)
			// OVS port for vpn/internet is the lab iface name (not DevicePortKey hash).
			synth.Name = fmt.Sprintf("%s-%s", conn.Spec.LabRef, ep.Device)
			eps = append(eps, epInfo{ep, synth, false, true})
			continue
		}

		deviceName := fmt.Sprintf("%s-%s", conn.Spec.LabRef, ep.Device)
		var dev laboratoryv1alpha1.Device
		if err := r.Get(ctx, types.NamespacedName{Name: deviceName, Namespace: conn.Namespace}, &dev); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		isSwitch := dev.Spec.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch ||
			dev.Spec.Type == laboratoryv1alpha1.DeviceTypeHub
		nodeReady := isSwitch || dev.Status.NodeName != ""
		if !nodeReady {
			needsRequeue = true
		}
		eps = append(eps, epInfo{ep, dev, isSwitch, nodeReady})
	}

	if needsRequeue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Add finalizer before any OVS work so cleanup runs even if we crash mid-reconcile.
	if !controllerutil.ContainsFinalizer(conn, names.FinalizerOVSCleanup) {
		controllerutil.AddFinalizer(conn, names.FinalizerOVSCleanup)
		if err := r.Update(ctx, conn); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: conn.Name, Namespace: conn.Namespace}, conn); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Determine VNI.
	var vni uint
	for _, ep := range eps {
		if ep.isSwitch && ep.device.Status.VNI != nil {
			vni = *ep.device.Status.VNI
			break
		}
	}
	if vni == 0 && conn.Status.VNI != nil {
		vni = *conn.Status.VNI
	}
	if vni == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	desired := make([]laboratoryv1alpha1.ConnectionPortStatus, 0, len(eps))

	// switch↔switch — materialise the patch-pair locally (spec §7: "Patch —
	// всегда внутри ноды"). Each node runs the same logic and gets its own
	// local pair so that cross-VNI L2 transit works wherever members live.
	if len(eps) == 2 && eps[0].isSwitch && eps[1].isSwitch {
		nameA := patchPortName(conn.Name, eps[0].endpoint.Device)
		nameB := patchPortName(conn.Name, eps[1].endpoint.Device)
		if err := r.OVS.AddPatchPair(nameA, nameB); err != nil {
			return ctrl.Result{}, fmt.Errorf("add patch pair: %w", err)
		}
		// Flow rules that bind each patch port to its switch's VNI live in
		// the 7-table pipeline (REQ-NA-030..038), not yet implemented. The
		// patch wires exist; they will start carrying L2 once that lands.
		for _, ep := range eps {
			desired = append(desired, laboratoryv1alpha1.ConnectionPortStatus{
				Device:    ep.endpoint.Device,
				Interface: ep.endpoint.Interface,
				Connected: true,
			})
		}
		if reflect.DeepEqual(conn.Status.Ports, desired) {
			return ctrl.Result{}, nil
		}
		conn.Status.Ports = desired
		return ctrl.Result{}, r.Status().Update(ctx, conn)
	}

	for _, ep := range eps {
		portStatus := laboratoryv1alpha1.ConnectionPortStatus{
			Device:    ep.endpoint.Device,
			Interface: ep.endpoint.Interface,
		}

		if ep.isSwitch {
			portStatus.Connected = true
			desired = append(desired, portStatus)
			continue
		}

		portStatus.NodeName = ep.device.Status.NodeName
		portStatus.NodeAddress = ep.device.Status.NodeAddress

		if ep.device.Status.NodeName == r.NodeName {
			// Virtual singletons (vpn, internet) use a fixed OVS port named by LabIfaceName,
			// not the hash-based DevicePortKey used for regular container/vm devices.
			var pKey string
			if ep.endpoint.Device == "vpn" {
				pKey = names.LabIfaceName(conn.Spec.LabRef)
			} else if ep.endpoint.Device == "internet" {
				pKey = names.LabGWIfaceName(conn.Spec.LabRef)
			} else {
				pKey = names.DevicePortKey(conn.Namespace, ep.device.Name, ep.endpoint.Interface)
			}

			// Wait for NetworkAttachReconciler to create the OVS port before programming flows.
			exists, err := r.OVS.PortExists(pKey)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("check OVS port %q: %w", pKey, err)
			}
			if !exists {
				return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
			}

			// Program Geneve tunnels for remote endpoints. One shared Geneve
			// port per node; tun_dst is set per-flow.
			for _, remote := range eps {
				if remote.isSwitch || remote.device.Status.NodeName == r.NodeName || remote.device.Status.NodeAddress == "" {
					continue
				}
				if err := r.OVS.AddGenevePort("", ""); err != nil {
					return ctrl.Result{}, fmt.Errorf("ensure geneve port: %w", err)
				}
				gvPort := genevePortName(remote.device.Status.NodeAddress)
				if err := r.Flows.AddEgressFlow(pKey, gvPort, vni, remote.device.Status.NodeAddress); err != nil {
					return ctrl.Result{}, err
				}
				if err := r.Flows.AddIngressFlow(gvPort, pKey, vni); err != nil {
					return ctrl.Result{}, err
				}
			}

			// Program local switch flow (same-node VNI flooding).
			if err := r.Flows.AddLocalSwitchFlow(pKey, vni); err != nil {
				return ctrl.Result{}, err
			}

			portStatus.PortID = pKey
			portStatus.Connected = true
		} else {
			// Preserve existing portID/connected state for remote endpoints.
			for _, existing := range conn.Status.Ports {
				if existing.Device == ep.endpoint.Device && existing.Interface == ep.endpoint.Interface {
					portStatus.PortID = existing.PortID
					portStatus.Connected = existing.Connected
					break
				}
			}
		}

		desired = append(desired, portStatus)
	}

	if reflect.DeepEqual(conn.Status.Ports, desired) {
		return ctrl.Result{}, nil
	}

	conn.Status.Ports = desired
	return ctrl.Result{}, r.Status().Update(ctx, conn)
}

func (r *ConnectionReconciler) reconcileDelete(ctx context.Context, conn *laboratoryv1alpha1.Connection) (ctrl.Result, error) {
	for _, port := range conn.Status.Ports {
		if port.NodeName != r.NodeName || port.PortID == "" {
			continue
		}
		_ = r.Flows.DelFlowsByPort(port.PortID)
		_ = r.OVS.DelPort(port.PortID)
	}

	// switch↔switch patch ports are local to every node — clean ours up.
	if len(conn.Spec.Endpoints) == 2 {
		var devs []string
		for _, ep := range conn.Spec.Endpoints {
			devs = append(devs, ep.Device)
		}
		if len(devs) == 2 {
			_ = r.OVS.DelPort(patchPortName(conn.Name, devs[0]))
			_ = r.OVS.DelPort(patchPortName(conn.Name, devs[1]))
		}
	}

	if conn.Status.VNI != nil {
		for _, port := range conn.Status.Ports {
			if port.NodeName == r.NodeName || port.NodeAddress == "" {
				continue
			}
			gvPort := genevePortName(port.NodeAddress)
			_ = r.Flows.DelFlowsByVNI(*conn.Status.VNI, gvPort)
		}
	}

	controllerutil.RemoveFinalizer(conn, names.FinalizerOVSCleanup)
	return ctrl.Result{}, r.Update(ctx, conn)
}

// nodeAddressForNode returns the cached NodeAddress for the given node name by
// checking Device status records. Falls back to empty string (Geneve skipped for same-node).
func (r *ConnectionReconciler) nodeAddressForNode(nodeName string) string {
	if nodeName == r.NodeName {
		return r.NodeAddress
	}
	// For remote nodes, look up any device that runs there to get its NodeAddress.
	var devList laboratoryv1alpha1.DeviceList
	if err := r.List(context.Background(), &devList); err != nil {
		return ""
	}
	for _, d := range devList.Items {
		if d.Status.NodeName == nodeName && d.Status.NodeAddress != "" {
			return d.Status.NodeAddress
		}
	}
	return ""
}

func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Connection{}).
		Watches(
			&laboratoryv1alpha1.Device{},
			handler.EnqueueRequestsFromMapFunc(r.connectionsForDevice),
		).
		Complete(r)
}

func (r *ConnectionReconciler) connectionsForDevice(ctx context.Context, obj client.Object) []reconcile.Request {
	dev := obj.(*laboratoryv1alpha1.Device)
	var connList laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &connList, client.InNamespace(dev.Namespace),
		client.MatchingLabels{names.LabelLab: dev.Spec.LabRef}); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, conn := range connList.Items {
		for _, ep := range conn.Spec.Endpoints {
			if ep.Device == dev.Spec.Name {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: conn.Name, Namespace: conn.Namespace},
				})
				break
			}
		}
	}
	return reqs
}
