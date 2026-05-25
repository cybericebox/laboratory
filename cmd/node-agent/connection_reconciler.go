//go:build linux

package main

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// ConnectionReconciler programs br-ovs based on Connection CRDs.
type ConnectionReconciler struct {
	client.Client
	NodeName    string
	NodeAddress string
	OVS         *OVSManager
	Flows       *FlowManager
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
	if !controllerutil.ContainsFinalizer(conn, laboratoryv1alpha1.FinalizerOVSCleanup) {
		controllerutil.AddFinalizer(conn, laboratoryv1alpha1.FinalizerOVSCleanup)
		if err := r.Update(ctx, conn); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: conn.Name, Namespace: conn.Namespace}, conn); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
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
			pKey := portKey(conn.Namespace, conn.Name, ep.endpoint.Interface)

			if err := r.OVS.AddInternalPort(pKey); err != nil {
				return ctrl.Result{}, fmt.Errorf("add OVS port %q: %w", pKey, err)
			}

			netnsPath, err := FindPodNetNS(string(ep.device.UID))
			if err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			// Move port to pod netns (idempotent — ignore if already moved).
			_ = MoveToNetNS(pKey, netnsPath)

			// Rename inside netns to desired interface name.
			if ep.endpoint.Interface != "" && ep.endpoint.Interface != pKey {
				_ = RenameInNetNS(netnsPath, pKey, ep.endpoint.Interface)
			}

			// Configure IP/MAC from device spec if available.
			var mac, cidr string
			for _, iface := range ep.device.Spec.Interfaces {
				if iface.Name == ep.endpoint.Interface {
					mac = iface.MAC
					if iface.Addr.Type == laboratoryv1alpha1.AddrTypeStatic {
						cidr = iface.Addr.IP
					}
					break
				}
			}
			if mac != "" || cidr != "" {
				ifName := ep.endpoint.Interface
				if ifName == "" {
					ifName = pKey
				}
				_ = ConfigureInNetNS(netnsPath, ifName, mac, cidr)
			}

			// Program Geneve tunnels for remote endpoints.
			for _, remote := range eps {
				if remote.isSwitch || remote.device.Status.NodeName == r.NodeName || remote.device.Status.NodeAddress == "" {
					continue
				}
				gvPort := genevePortName(remote.device.Status.NodeAddress)
				if err := r.OVS.AddGenevePort(gvPort, remote.device.Status.NodeAddress); err != nil {
					return ctrl.Result{}, fmt.Errorf("add geneve port %q: %w", gvPort, err)
				}
				if err := r.Flows.AddEgressFlow(pKey, gvPort, vni); err != nil {
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

	if conn.Status.VNI != nil {
		for _, port := range conn.Status.Ports {
			if port.NodeName == r.NodeName || port.NodeAddress == "" {
				continue
			}
			gvPort := genevePortName(port.NodeAddress)
			_ = r.Flows.DelFlowsByVNI(*conn.Status.VNI, gvPort)
		}
	}

	controllerutil.RemoveFinalizer(conn, laboratoryv1alpha1.FinalizerOVSCleanup)
	return ctrl.Result{}, r.Update(ctx, conn)
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
		client.MatchingLabels{laboratoryv1alpha1.LabelLab: dev.Spec.LabRef}); err != nil {
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
