//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// ConnectionReconciler programs br-ovs based on Connection CRDs.
//
// Three connection scenarios (spec §8):
//
//  1. Device↔Device   — both ports in connVNI; t6 floods within connVNI.
//  2. Device↔Switch   — device port enters SW_VNI; t6 is rebuilt by aggregating
//     ALL connections to that switch (devices on any node).
//  3. Switch↔Switch   — local patch-pair; each end registers in its switch's VNI;
//     t6 for each switch VNI is rebuilt to include the patch port.
//
// Pod migration is handled by the DevicePortReconciler stamping a new NodeAddress
// on the Device, which triggers connectionsForDevice → requeue this reconciler.
// reconcileCreate is fully idempotent: it removes and re-adds t0 before setting
// a new value, and atomically replaces the t6 flood entry for the affected VNI.
type ConnectionReconciler struct {
	client.Client
	NodeName    string
	NodeAddress string
	OVS         *OVSManager
	Flows       *FlowManager
	Recorder    record.EventRecorder
}

// warnf emits a Warning event on the Connection if a recorder is configured.
// Used to surface programming failures that would otherwise be silent requeues.
func (r *ConnectionReconciler) warnf(conn *laboratoryv1alpha1.Connection, reason, format string, args ...interface{}) {
	if r.Recorder != nil {
		r.Recorder.Eventf(conn, corev1.EventTypeWarning, reason, format, args...)
	}
}

// epInfo holds resolved endpoint state for one reconcile cycle.
type epInfo struct {
	endpoint  laboratoryv1alpha1.EndpointSpec
	device    laboratoryv1alpha1.Device
	isSwitch  bool
	nodeReady bool
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

// reconcileCreate dispatches to the appropriate scenario handler after loading
// endpoints and ensuring the shared Geneve port + its t0 ingress entry exist.
func (r *ConnectionReconciler) reconcileCreate(ctx context.Context, conn *laboratoryv1alpha1.Connection) (
	ctrl.Result,
	error,
) {
	eps, needsRequeue, err := r.loadEndpoints(ctx, conn)
	if err != nil {
		return ctrl.Result{}, err
	}
	if needsRequeue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if !controllerutil.ContainsFinalizer(conn, names.FinalizerOVSCleanup) {
		controllerutil.AddFinalizer(conn, names.FinalizerOVSCleanup)
		if err := r.Update(ctx, conn); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: conn.Name, Namespace: conn.Namespace}, conn); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Ensure shared Geneve port and its permanent t0 rule.
	if err := r.OVS.AddGenevePort("", ""); err != nil {
		r.warnf(conn, labstatus.ReasonProgrammingFailed, "ensure Geneve port failed: %v", err)
		return ctrl.Result{}, fmt.Errorf("ensure geneve port: %w", err)
	}
	if err := r.Flows.InitGeneveIngress(); err != nil {
		r.warnf(conn, labstatus.ReasonProgrammingFailed, "program Geneve ingress flow failed: %v", err)
		return ctrl.Result{}, fmt.Errorf("init geneve ingress: %w", err)
	}

	sw0, sw1 := eps[0].isSwitch, eps[1].isSwitch
	var res ctrl.Result
	switch {
	case sw0 && sw1:
		res, err = r.reconcileSwitchSwitch(ctx, conn, eps)
	case sw0 || sw1:
		res, err = r.reconcileDeviceSwitch(ctx, conn, eps)
	default:
		res, err = r.reconcileDeviceDevice(ctx, conn, eps)
	}
	// Self-healing resync: veth recovery paths (SetupNetworks, NetworkAttach)
	// can delete and re-create ports, after which OVS assigns new ofport numbers.
	// Nothing requeues this Connection on such events, so periodically re-program
	// t0/t6 (idempotent) to converge flows onto the current numbers.
	if err == nil && res.IsZero() {
		res.RequeueAfter = 60 * time.Second
	}
	return res, err
}

// reconcileDeviceDevice handles direct device-to-device connections.
// Both device ports go into connVNI; t6 floods within that VNI.
func (r *ConnectionReconciler) reconcileDeviceDevice(
	ctx context.Context,
	conn *laboratoryv1alpha1.Connection,
	eps []epInfo,
) (ctrl.Result, error) {
	var vni uint
	if conn.Status.VNI != nil {
		vni = *conn.Status.VNI
	}
	if vni == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	desired := make([]laboratoryv1alpha1.ConnectionPortStatus, 0, len(eps))
	var localPorts []string
	seenVTEPs := make(map[string]struct{})
	var remoteVTEPs []string

	for _, ep := range eps {
		portStatus := laboratoryv1alpha1.ConnectionPortStatus{
			Device:      ep.endpoint.Device,
			Interface:   ep.endpoint.Interface,
			NodeName:    ep.device.Status.NodeName,
			NodeAddress: ep.device.Status.NodeAddress,
		}

		if ep.device.Status.NodeName == r.NodeName {
			pKey, requeue, err := r.resolveLocalPortKey(ctx, conn, ep)
			if err != nil {
				return ctrl.Result{}, err
			}
			if requeue {
				return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
			}
			exists, err := r.OVS.PortExists(pKey)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("check port %q: %w", pKey, err)
			}
			if !exists {
				return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
			}

			if err := r.Flows.AddT0Port(pKey, vni); err != nil {
				return ctrl.Result{}, err
			}
			localPorts = append(localPorts, pKey)
			portStatus.PortID = pKey
			portStatus.Connected = true
		} else {
			for _, existing := range conn.Status.Ports {
				if existing.Device == ep.endpoint.Device && existing.Interface == ep.endpoint.Interface {
					portStatus.PortID = existing.PortID
					portStatus.Connected = existing.Connected
					break
				}
			}
			if ep.device.Status.NodeAddress != "" {
				if _, seen := seenVTEPs[ep.device.Status.NodeAddress]; !seen {
					seenVTEPs[ep.device.Status.NodeAddress] = struct{}{}
					remoteVTEPs = append(remoteVTEPs, ep.device.Status.NodeAddress)
				}
			}
		}
		desired = append(desired, portStatus)
	}

	if len(localPorts) > 0 {
		if err := r.Flows.RebuildT6Flood(vni, localPorts, remoteVTEPs); err != nil {
			return ctrl.Result{}, err
		}
	}

	if reflect.DeepEqual(conn.Status.Ports, desired) {
		return ctrl.Result{}, nil
	}
	conn.Status.Ports = desired
	return ctrl.Result{}, r.Status().Update(ctx, conn)
}

// reconcileDeviceSwitch handles device-to-switch/hub connections.
// The device port enters the switch's VNI (SW_VNI); t6 for SW_VNI is rebuilt
// by aggregating all connections that share the same switch endpoint.
func (r *ConnectionReconciler) reconcileDeviceSwitch(
	ctx context.Context,
	conn *laboratoryv1alpha1.Connection,
	eps []epInfo,
) (ctrl.Result, error) {
	var swEp, devEp epInfo
	for _, ep := range eps {
		if ep.isSwitch {
			swEp = ep
		} else {
			devEp = ep
		}
	}

	var vni uint
	if swEp.device.Status.VNI != nil {
		vni = *swEp.device.Status.VNI
	}
	if vni == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	desired := make([]laboratoryv1alpha1.ConnectionPortStatus, 0, 2)
	desired = append(
		desired, laboratoryv1alpha1.ConnectionPortStatus{
			Device:    swEp.endpoint.Device,
			Interface: swEp.endpoint.Interface,
			Connected: true,
		},
	)

	portStatus := laboratoryv1alpha1.ConnectionPortStatus{
		Device:      devEp.endpoint.Device,
		Interface:   devEp.endpoint.Interface,
		NodeName:    devEp.device.Status.NodeName,
		NodeAddress: devEp.device.Status.NodeAddress,
	}

	var currentLocalPort string

	if devEp.device.Status.NodeName == r.NodeName {
		pKey, requeue, err := r.resolveLocalPortKey(ctx, conn, devEp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
		exists, err := r.OVS.PortExists(pKey)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("check port %q: %w", pKey, err)
		}
		if !exists {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}

		if err := r.Flows.AddT0Port(pKey, vni); err != nil {
			return ctrl.Result{}, err
		}
		currentLocalPort = pKey
		portStatus.PortID = pKey
		portStatus.Connected = true
	} else {
		for _, existing := range conn.Status.Ports {
			if existing.Device == devEp.endpoint.Device && existing.Interface == devEp.endpoint.Interface {
				portStatus.PortID = existing.PortID
				portStatus.Connected = existing.Connected
				break
			}
		}
	}
	desired = append(desired, portStatus)

	// Rebuild t6 for SW_VNI aggregating all connections to this switch.
	// Inject currentLocalPort in case Status.Ports hasn't been written yet.
	localPorts, remoteVTEPs, err := r.buildSwitchVNIFlood(ctx, conn.Namespace, conn.Spec.LabRef, swEp.endpoint.Device)
	if err != nil {
		return ctrl.Result{}, err
	}
	if currentLocalPort != "" {
		found := false
		for _, p := range localPorts {
			if p == currentLocalPort {
				found = true
				break
			}
		}
		if !found {
			localPorts = append(localPorts, currentLocalPort)
		}
	}
	if len(localPorts) > 0 {
		if err := r.Flows.RebuildT6Flood(vni, localPorts, remoteVTEPs); err != nil {
			return ctrl.Result{}, err
		}
	}

	if reflect.DeepEqual(conn.Status.Ports, desired) {
		return ctrl.Result{}, nil
	}
	conn.Status.Ports = desired
	return ctrl.Result{}, r.Status().Update(ctx, conn)
}

// reconcileSwitchSwitch handles switch-to-switch connections.
// A patch-pair is created on every node (pair is intra-node per spec §7).
// Each patch port is registered in its switch's VNI via t0, and t6 for both
// switch VNIs is rebuilt to include the patch ports and all member devices.
func (r *ConnectionReconciler) reconcileSwitchSwitch(
	ctx context.Context,
	conn *laboratoryv1alpha1.Connection,
	eps []epInfo,
) (ctrl.Result, error) {
	ep0, ep1 := eps[0], eps[1]

	var vni0, vni1 uint
	if ep0.device.Status.VNI != nil {
		vni0 = *ep0.device.Status.VNI
	}
	if ep1.device.Status.VNI != nil {
		vni1 = *ep1.device.Status.VNI
	}
	if vni0 == 0 || vni1 == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Patch port names: each end lives in its switch's VNI domain.
	patchA := patchPortName(conn.Namespace, conn.Name, ep0.endpoint.Device) // registered in vni0
	patchB := patchPortName(conn.Namespace, conn.Name, ep1.endpoint.Device) // registered in vni1

	// A pair under the names of earlier versions (no namespace in them) is replaced by this one: leaving it would flood each switch's
	// frames through two links.
	for _, dev := range []string{ep0.endpoint.Device, ep1.endpoint.Device} {
		old := legacyPatchPortName(conn.Name, dev)
		if exists, err := r.OVS.PortExists(old); err == nil && exists {
			_ = r.Flows.DelT0Port(old)
			_ = r.OVS.DelPort(old)
		}
	}

	if err := r.OVS.AddPatchPair(patchA, patchB); err != nil {
		return ctrl.Result{}, fmt.Errorf("add patch pair: %w", err)
	}

	// Bind each patch port to its switch's VNI in t0 (idempotent via AddT0Port).
	if err := r.Flows.AddT0Port(patchA, vni0); err != nil {
		return ctrl.Result{}, fmt.Errorf("t0 %s→VNI%d: %w", patchA, vni0, err)
	}
	if err := r.Flows.AddT0Port(patchB, vni1); err != nil {
		return ctrl.Result{}, fmt.Errorf("t0 %s→VNI%d: %w", patchB, vni1, err)
	}

	// Rebuild t6 for both switch VNIs (patches are now in OVS; buildSwitchVNIFlood
	// will find them via PortExists).
	localPorts0, remoteVTEPs0, err := r.buildSwitchVNIFlood(ctx, conn.Namespace, conn.Spec.LabRef, ep0.endpoint.Device)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(localPorts0) > 0 {
		if err := r.Flows.RebuildT6Flood(vni0, localPorts0, remoteVTEPs0); err != nil {
			return ctrl.Result{}, err
		}
	}

	localPorts1, remoteVTEPs1, err := r.buildSwitchVNIFlood(ctx, conn.Namespace, conn.Spec.LabRef, ep1.endpoint.Device)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(localPorts1) > 0 {
		if err := r.Flows.RebuildT6Flood(vni1, localPorts1, remoteVTEPs1); err != nil {
			return ctrl.Result{}, err
		}
	}

	desired := []laboratoryv1alpha1.ConnectionPortStatus{
		{Device: ep0.endpoint.Device, Interface: ep0.endpoint.Interface, Connected: true},
		{Device: ep1.endpoint.Device, Interface: ep1.endpoint.Interface, Connected: true},
	}
	if reflect.DeepEqual(conn.Status.Ports, desired) {
		return ctrl.Result{}, nil
	}
	conn.Status.Ports = desired
	return ctrl.Result{}, r.Status().Update(ctx, conn)
}

// buildSwitchVNIFlood collects all local OVS port names and remote VTEP IPs
// for a given switch's VNI domain by inspecting every connection in the lab
// that has switchLogicalName as an endpoint.
//
// Device↔Switch connections: port IDs are read from conn.Status.Ports.
// Switch↔Switch connections: the local patch port is checked via OVS PortExists.
//
// Connections with a non-nil DeletionTimestamp are skipped so that in-progress
// deletions do not appear in the rebuilt flood list.
func (r *ConnectionReconciler) buildSwitchVNIFlood(
	ctx context.Context,
	namespace, labRef, switchLogicalName string,
) (localPorts, remoteVTEPs []string, _ error) {
	var connList laboratoryv1alpha1.ConnectionList
	if err := r.List(
		ctx, &connList,
		client.InNamespace(namespace),
		client.MatchingLabels{names.LabelLab: labRef},
	); err != nil {
		return nil, nil, err
	}

	seenVTEPs := make(map[string]struct{})

	for i := range connList.Items {
		c := &connList.Items[i]
		if c.DeletionTimestamp != nil {
			continue
		}

		// Skip connections that don't involve our switch.
		hasSw := false
		for _, ep := range c.Spec.Endpoints {
			if ep.Device == switchLogicalName {
				hasSw = true
				break
			}
		}
		if !hasSw {
			continue
		}

		// Determine whether all other endpoints are also switches.
		allOtherSwitches := true
		for _, ep := range c.Spec.Endpoints {
			if ep.Device == switchLogicalName {
				continue
			}
			found, err := devices.Get(ctx, r.Client, namespace, labRef, ep.Device)
			if err != nil {
				allOtherSwitches = false
				break
			}
			dev := *found
			if dev.Spec.Type != laboratoryv1alpha1.DeviceTypeUnmanagedSwitch &&
				dev.Spec.Type != laboratoryv1alpha1.DeviceTypeHub {
				allOtherSwitches = false
				break
			}
		}

		if allOtherSwitches {
			// Switch↔Switch: include patch port on our switch's side if it exists.
			pName := patchPortName(c.Namespace, c.Name, switchLogicalName)
			if exists, err := r.OVS.PortExists(pName); err == nil && exists {
				localPorts = append(localPorts, pName)
			}
		} else {
			// Device↔Switch: read recorded port IDs from Status.Ports.
			for _, sp := range c.Status.Ports {
				if sp.Device == switchLogicalName || sp.PortID == "" {
					continue
				}
				if sp.NodeName == r.NodeName {
					if exists, _ := r.OVS.PortExists(sp.PortID); exists {
						localPorts = append(localPorts, sp.PortID)
					}
				} else if sp.NodeAddress != "" {
					if _, seen := seenVTEPs[sp.NodeAddress]; !seen {
						seenVTEPs[sp.NodeAddress] = struct{}{}
						remoteVTEPs = append(remoteVTEPs, sp.NodeAddress)
					}
				}
			}
		}
	}

	return localPorts, remoteVTEPs, nil
}

func (r *ConnectionReconciler) reconcileDelete(ctx context.Context, conn *laboratoryv1alpha1.Connection) (
	ctrl.Result,
	error,
) {
	// Remove local device ports from OVS and their t0 entries.
	for _, port := range conn.Status.Ports {
		if port.NodeName != r.NodeName || port.PortID == "" {
			continue
		}
		_ = r.Flows.DelT0Port(port.PortID)
		_ = r.OVS.DelPort(port.PortID)
	}

	// Remove t6 flood for the connection's own VNI (device↔device case).
	if conn.Status.VNI != nil {
		_ = r.Flows.DelT6Flood(*conn.Status.VNI)
	}

	// For each switch endpoint: remove patch port + rebuild t6 from remaining connections.
	for _, ep := range conn.Spec.Endpoints {
		found, err := devices.Get(ctx, r.Client, conn.Namespace, conn.Spec.LabRef, ep.Device)
		if err != nil {
			continue
		}
		dev := *found
		isSwitch := dev.Spec.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch ||
			dev.Spec.Type == laboratoryv1alpha1.DeviceTypeHub
		if !isSwitch {
			continue
		}

		// Remove patch port for this side (switch↔switch case).
		pName := patchPortName(conn.Namespace, conn.Name, ep.Device)
		_ = r.Flows.DelT0Port(pName)
		_ = r.OVS.DelPort(pName)

		// Rebuild t6 for this switch's VNI from the remaining connections.
		if dev.Status.VNI != nil {
			localPorts, remoteVTEPs, err := r.buildSwitchVNIFlood(ctx, conn.Namespace, conn.Spec.LabRef, ep.Device)
			if err == nil {
				if len(localPorts) > 0 {
					_ = r.Flows.RebuildT6Flood(*dev.Status.VNI, localPorts, remoteVTEPs)
				} else {
					_ = r.Flows.DelT6Flood(*dev.Status.VNI)
				}
			}
		}
	}

	controllerutil.RemoveFinalizer(conn, names.FinalizerOVSCleanup)
	return ctrl.Result{}, r.Update(ctx, conn)
}

// loadEndpoints fetches Device objects for all endpoints in a connection and
// returns them as []epInfo. needsRequeue=true means at least one endpoint is not
// yet ready (pod not scheduled or VNI not allocated); the caller should requeue.
func (r *ConnectionReconciler) loadEndpoints(ctx context.Context, conn *laboratoryv1alpha1.Connection) (
	[]epInfo,
	bool,
	error,
) {
	eps := make([]epInfo, 0, len(conn.Spec.Endpoints))
	needsRequeue := false

	for _, ep := range conn.Spec.Endpoints {
		// Virtual singletons (vpn, internet) have no Device CRD — synthesize.
		if ep.Device == "vpn" || ep.Device == "internet" {
			// The "internet" endpoint is served by the gateway Deployment,
			// whose pods carry the gateway component label.
			component := ep.Device
			if ep.Device == "internet" {
				component = names.ComponentGateway
			}
			var pods corev1.PodList
			if err := r.List(
				ctx, &pods,
				client.InNamespace(conn.Namespace),
				client.MatchingLabels{names.LabelComponent: component},
				client.Limit(1),
			); err != nil {
				return nil, false, err
			}
			if len(pods.Items) == 0 {
				// A pod made before the component label: it is the one with app=<component> that is no device's.
				req, err := labels.NewRequirement(names.LabelLab, selection.DoesNotExist, nil)
				if err != nil {
					return nil, false, err
				}
				if err := r.List(
					ctx, &pods,
					client.InNamespace(conn.Namespace),
					client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{"app": component}).Add(*req)},
					client.Limit(1),
				); err != nil {
					return nil, false, err
				}
			}
			if len(pods.Items) == 0 || pods.Items[0].Spec.NodeName == "" {
				needsRequeue = true
				eps = append(eps, epInfo{ep, laboratoryv1alpha1.Device{}, false, false})
				continue
			}
			pod := pods.Items[0]
			synth := laboratoryv1alpha1.Device{}
			synth.Spec.Type = laboratoryv1alpha1.DeviceTypeContainer
			synth.Status.NodeName = pod.Spec.NodeName
			synth.Status.NodeAddress = r.nodeAddressForNode(ctx, pod.Spec.NodeName)
			synth.Name = fmt.Sprintf("%s-%s", conn.Spec.LabRef, ep.Device)
			eps = append(eps, epInfo{ep, synth, false, true})
			continue
		}

		found, err := devices.Get(ctx, r.Client, conn.Namespace, conn.Spec.LabRef, ep.Device)
		if err != nil {
			return nil, false, client.IgnoreNotFound(err)
		}
		dev := *found
		isSwitch := dev.Spec.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch ||
			dev.Spec.Type == laboratoryv1alpha1.DeviceTypeHub
		nodeReady := isSwitch || dev.Status.NodeName != ""
		if !nodeReady {
			needsRequeue = true
		}
		eps = append(eps, epInfo{ep, dev, isSwitch, nodeReady})
	}

	return eps, needsRequeue, nil
}

// resolveLocalPortKey returns the OVS port key for a local endpoint.
// For vpn/internet singletons the key comes from the LabVPN/LabGateway CRD;
// for regular devices it is the per-pod DevicePortKey of the device's CURRENT
// pod (Device Status.PodName). Because a device's pod is a Deployment replica
// whose name changes on recreation, the key is resolved from the live status,
// not the Device name — so when the pod is replaced this reconciler rebinds the
// VNI onto the new pod's port (the old port drops out with the terminating pod).
func (r *ConnectionReconciler) resolveLocalPortKey(
	ctx context.Context,
	conn *laboratoryv1alpha1.Connection,
	ep epInfo,
) (pKey string, requeue bool, err error) {
	switch ep.endpoint.Device {
	case "vpn":
		var labvpn laboratoryv1alpha1.LabVPN
		if err := r.Get(
			ctx, types.NamespacedName{
				Name:      names.LabVPNObjectName(conn.Spec.LabRef),
				Namespace: conn.Namespace,
			}, &labvpn,
		); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return "", false, err
			}
			return "", true, nil
		}
		return names.VPNHostPortKey(conn.Namespace, labvpn.Spec.NetworkIndex), false, nil
	case "internet":
		var labgw laboratoryv1alpha1.LabGateway
		if err := r.Get(
			ctx, types.NamespacedName{
				Name:      names.LabGatewayObjectName(conn.Spec.LabRef),
				Namespace: conn.Namespace,
			}, &labgw,
		); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return "", false, err
			}
			return "", true, nil
		}
		return names.GWHostPortKey(conn.Namespace, labgw.Spec.NetworkIndex), false, nil
	default:
		// The device's pod must exist before its port can be wired; requeue until
		// the operator publishes the current pod name.
		if ep.device.Status.PodName == "" {
			return "", true, nil
		}
		return names.DevicePortKey(conn.Namespace, ep.device.Status.PodName, ep.endpoint.Interface), false, nil
	}
}

// nodeAddressForNode returns the Geneve VTEP address for the given node name:
// the node's InternalIP, which is what every node-agent uses as its own VTEP
// (NODE_ADDRESS = status.hostIP). Device statuses are only a fallback: a vpn or
// gateway pod may sit on a node where no Device of any lab runs yet, and an
// empty address leaves that side without the tunnel back (one-way traffic).
func (r *ConnectionReconciler) nodeAddressForNode(ctx context.Context, nodeName string) string {
	if nodeName == r.NodeName {
		return r.NodeAddress
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err == nil {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
				return addr.Address
			}
		}
	}
	var devList laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &devList); err != nil {
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
	if err := r.List(
		ctx, &connList, client.InNamespace(dev.Namespace),
		client.MatchingLabels{names.LabelLab: dev.Spec.LabRef},
	); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, conn := range connList.Items {
		for _, ep := range conn.Spec.Endpoints {
			if ep.Device == dev.Spec.Name {
				reqs = append(
					reqs, reconcile.Request{
						NamespacedName: types.NamespacedName{Name: conn.Name, Namespace: conn.Namespace},
					},
				)
				break
			}
		}
	}
	return reqs
}
