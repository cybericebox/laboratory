package laboratory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	labstatus "github.com/cybericebox/laboratory/internal/status"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/cybericebox/laboratory/pkg/netutil"
)

// LabReconciler reconciles a Lab object.
type LabReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// BaseDomain is the public DNS suffix under which task URLs are advertised,
	// e.g. "challenges.cybericebox.com". An exposed device named "web" inside a
	// lab is reachable as https://web-<code>.<BaseDomain> (names.WebHostLabel; code is 3-4 random
	// base36 chars fixed by the name of the device's Service). Written to
	// Lab.Status.Access on Ready.
	BaseDomain string
	// ProxySourceCIDRs is an optional list of CIDRs added as ipBlock peers in the
	// web-exposure NetworkPolicy. Required when the proxy runs with hostNetwork (its
	// source IP is the node IP, not a pod IP, so namespace/label selectors don't apply).
	ProxySourceCIDRs []string
	// newWebCode draws the random code of a web host label; nil means
	// names.NewWebCode. A field so tests can force collisions.
	newWebCode func(n int) (string, error)
	// VPNBaseNetwork is the base address space for per-lab VPN subnets (e.g. "10.8.0.0/16").
	VPNBaseNetwork string
	// InetBaseNetwork is the base address space for per-lab internet/gateway subnets (e.g. "10.9.0.0/16").
	InetBaseNetwork string
	// LaunchGate holds a new Lab back until the Launcher admits it (launch pacing).
	// Off: the lab is provisioned as soon as it is created.
	LaunchGate bool
	// State is the device state persistence policy applied to labs created
	// while the platform switch is on.
	State StatePolicy
	// Mirror rewrites image references for the image cache; the zero value
	// (cache off) rewrites nothing. The Lab records the decision once.
	Mirror imagecache.Rewriter
	// Resolver pins image tags to digests for a lab created with the image
	// cache on; nil pins nothing.
	Resolver imagecache.Resolver
	// NetConfigImage is the image of the device netconfig init-container, pinned
	// together with the device images.
	NetConfigImage string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/finalizers,verbs=update
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices;connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices/status;connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labvpns;labgateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labvpns/status;labgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labvpns/finalizers;labgateways/finalizers,verbs=update

func (r *LabReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("reconciling Lab", "name", req.Name, "namespace", req.Namespace)

	var lab laboratoryv1alpha1.Lab
	if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lab.DeletionTimestamp.IsZero() {
		logger.Info("lab is being deleted", "name", lab.Name)
		return r.reconcileDelete(ctx, &lab)
	}

	if !controllerutil.ContainsFinalizer(&lab, names.FinalizerLab) {
		controllerutil.AddFinalizer(&lab, names.FinalizerLab)
		if err := r.Update(ctx, &lab); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The modes are fixed before anything is created, and before the queue, so
	// the launcher knows which image references the lab will pull.
	if updated, err := r.ensureModes(ctx, &lab); err != nil {
		return ctrl.Result{}, err
	} else if updated {
		if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// A queued lab creates nothing yet: the launcher admits it (status patch),
	// which triggers the next reconcile.
	if r.LaunchGate && !labAdmitted(&lab) {
		return ctrl.Result{}, nil
	}

	if updated, err := r.ensureSubnetAllocation(ctx, &lab); err != nil {
		return ctrl.Result{}, err
	} else if updated {
		// Re-fetch after status update so we have the latest resourceVersion.
		if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if err := r.ensureLabNetworkObjects(ctx, &lab); err != nil {
		return ctrl.Result{}, err
	}

	validationErr := r.validateGraph(&lab)
	var resolvedInterfaces map[string][]laboratoryv1alpha1.InterfaceSpec
	if validationErr == nil {
		resolvedInterfaces, validationErr = resolveLabDeviceInterfaces(&lab)
	}
	if validationErr != nil {
		// Surface the specific validation reason — it was previously discarded,
		// leaving the Lab in Failed with no user-visible cause.
		lab.Status.Phase = laboratoryv1alpha1.PhaseFailed
		labstatus.SetReady(
			&lab.Status.Conditions, lab.Generation, false,
			labstatus.ReasonValidationFailed, validationErr.Error(),
		)
		r.Recorder.Event(&lab, corev1.EventTypeWarning, labstatus.ReasonValidationFailed, validationErr.Error())
		if statusErr := r.Status().Update(ctx, &lab); statusErr != nil {
			logger.Error(statusErr, "update status after graph validation failure")
		}
		return ctrl.Result{}, nil
	}

	// After validation: the web host label relies on validated device names.
	if err := r.ensureWebServices(ctx, &lab); err != nil {
		logger.Error(err, "ensure web services")
		return ctrl.Result{}, err
	}

	if err := r.materializeDevices(ctx, &lab, resolvedInterfaces); err != nil {
		logger.Error(err, "materialize devices")
		return ctrl.Result{}, err
	}

	if err := r.materializeConnections(ctx, &lab); err != nil {
		logger.Error(err, "materialize connections")
		return ctrl.Result{}, err
	}

	// Prune what the spec no longer wants (edit = add via materialize + remove
	// via prune). Connections first so a removed device's link is gone before
	// the device itself.
	if err := r.pruneConnections(ctx, &lab); err != nil {
		logger.Error(err, "prune connections")
		return ctrl.Result{}, err
	}
	if err := r.pruneDevices(ctx, &lab); err != nil {
		logger.Error(err, "prune devices")
		return ctrl.Result{}, err
	}

	if err := r.ensureDeploymentAnnotations(ctx, &lab); err != nil {
		logger.Error(err, "ensure deployment annotations")
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, &lab)
}

// validateGraph checks device names, endpoint ports, occupancy and switch/hub cycles.
func (r *LabReconciler) validateGraph(lab *laboratoryv1alpha1.Lab) error {
	for _, d := range lab.Spec.Devices {
		if err := names.ValidateDeviceName(d.Name); err != nil {
			return fmt.Errorf("InvalidDeviceName: %w", err)
		}
	}
	switchDevices := map[string]bool{}
	deviceIfaces := map[string]map[string]bool{}
	for _, d := range lab.Spec.Devices {
		if d.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch || d.Type == laboratoryv1alpha1.DeviceTypeHub {
			switchDevices[d.Name] = true
		}
		ifaces := make(map[string]bool, len(d.Interfaces))
		for _, iface := range d.Interfaces {
			ifaces[iface.Name] = true
		}
		deviceIfaces[d.Name] = ifaces
	}

	// Lab interface names must not shadow the reserved access-port name:
	// SetupNetworks would delete/replace the delegated interface on a CNI retry.
	for _, d := range lab.Spec.Devices {
		for _, iface := range d.Interfaces {
			if iface.Name == names.AccessPortIface {
				return fmt.Errorf(
					"ReservedInterfaceName: device %q uses reserved interface name %q",
					d.Name,
					iface.Name,
				)
			}
		}
	}

	// Every endpoint must reference a declared device. For container devices,
	// the endpoint interface must exist in the device's interface list.
	// The OVS port key is derived from (pod, interface) on one side and
	// (device, endpoint.Interface) on the other — a name mismatch would silently
	// program flows against a port that was never created.
	// A device interface is one veth in one VNI: it may appear in at most one
	// connection, otherwise the later t0 programming silently overwrites the earlier.
	usedIfaces := map[string]bool{}
	usedGateways := map[string]bool{}
	for _, conn := range lab.Spec.Connections {
		for _, ep := range conn.Endpoints {
			if ep.Device == "vpn" || ep.Device == "internet" {
				// Each singleton exposes exactly one logical port: eth0.
				if ep.Interface != "eth0" {
					return fmt.Errorf("InvalidGatewayPort: %s has no port %q", ep.Device, ep.Interface)
				}
				if usedGateways[ep.Device] {
					return fmt.Errorf("DuplicateGatewayPort: %s eth0 is used by more than one connection", ep.Device)
				}
				usedGateways[ep.Device] = true
				continue // virtual singletons have no Device template
			}
			ifaces, ok := deviceIfaces[ep.Device]
			if !ok {
				return fmt.Errorf(
					"UnknownEndpointDevice: connection endpoint references undeclared device %q",
					ep.Device,
				)
			}
			if switchDevices[ep.Device] {
				if !validForwardingPort(ep.Interface) {
					return fmt.Errorf("InvalidForwardingPort: device %q has no port %q", ep.Device, ep.Interface)
				}
			} else if !ifaces[ep.Interface] {
				return fmt.Errorf(
					"UnknownEndpointInterface: device %q has no interface %q declared",
					ep.Device,
					ep.Interface,
				)
			}
			key := ep.Device + "/" + ep.Interface
			if usedIfaces[key] {
				return fmt.Errorf(
					"DuplicateEndpointInterface: interface %q of device %q is used by more than one connection",
					ep.Interface,
					ep.Device,
				)
			}
			usedIfaces[key] = true
		}
	}

	adj := map[string][]string{}
	for _, conn := range lab.Spec.Connections {
		var switches []string
		for _, ep := range conn.Endpoints {
			if switchDevices[ep.Device] {
				switches = append(switches, ep.Device)
			}
		}
		for i := 0; i < len(switches); i++ {
			for j := i + 1; j < len(switches); j++ {
				adj[switches[i]] = append(adj[switches[i]], switches[j])
				adj[switches[j]] = append(adj[switches[j]], switches[i])
			}
		}
	}

	visited := map[string]bool{}
	var dfs func(node, parent string) bool
	dfs = func(node, parent string) bool {
		visited[node] = true
		for _, neighbor := range adj[node] {
			if neighbor == parent {
				continue
			}
			if visited[neighbor] || dfs(neighbor, node) {
				return true
			}
		}
		return false
	}

	for node := range switchDevices {
		if !visited[node] {
			if dfs(node, "") {
				return fmt.Errorf("SwitchCycleDetected: connection graph contains a switch/hub cycle")
			}
		}
	}
	return r.validateBroadcastDomains(lab, switchDevices)
}

func validForwardingPort(name string) bool {
	const prefix = "GigabitEthernet0/"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	return err == nil && number >= 1 && number <= 48 && name == fmt.Sprintf("%s%d", prefix, number)
}

// validateBroadcastDomains groups Connections into broadcast components
// (transitively merged through shared switch/hub endpoints) and rejects:
//   - REQ-OP-026: same domain contains both VPN and Internet singletons.
//   - REQ-OP-027: same domain has more than one DHCP source.
//
// A non-switch device (container) does not propagate the domain — it sits
// as a leaf in whichever single connection it appears.
func (r *LabReconciler) validateBroadcastDomains(lab *laboratoryv1alpha1.Lab, switchDevices map[string]bool) error {
	n := len(lab.Spec.Connections)
	if n == 0 {
		return nil
	}
	// Union-Find over Connection indices; merge through shared switch/hub.
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// switchEnds[device] = list of connection indices touching that switch.
	switchEnds := map[string][]int{}
	for ci, conn := range lab.Spec.Connections {
		for _, ep := range conn.Endpoints {
			if switchDevices[ep.Device] {
				switchEnds[ep.Device] = append(switchEnds[ep.Device], ci)
			}
		}
	}
	for _, conns := range switchEnds {
		for i := 1; i < len(conns); i++ {
			union(conns[0], conns[i])
		}
	}

	// Per-component flags.
	type domainFlags struct {
		hasVPN, hasInternet bool
		dhcpSources         int
	}
	domains := map[int]*domainFlags{}
	for ci, conn := range lab.Spec.Connections {
		root := find(ci)
		d, ok := domains[root]
		if !ok {
			d = &domainFlags{}
			domains[root] = d
		}
		for _, ep := range conn.Endpoints {
			switch ep.Device {
			case "vpn":
				d.hasVPN = true
				if lab.Spec.VPN.DHCPServer != nil && lab.Spec.VPN.DHCPServer.Enabled {
					d.dhcpSources++
				}
			case "internet":
				d.hasInternet = true
				if lab.Spec.Internet.DHCPServer != nil && lab.Spec.Internet.DHCPServer.Enabled {
					d.dhcpSources++
				}
			}
		}
	}

	for _, d := range domains {
		if d.hasVPN && d.hasInternet {
			return fmt.Errorf("BroadcastDomainSpansVPNAndInternet: VPN and Internet singletons must live in separate broadcast domains")
		}
		if d.dhcpSources > 1 {
			return fmt.Errorf(
				"MultipleDHCPServersInBroadcastDomain: at most one DHCP server is allowed per broadcast domain (found %d)",
				d.dhcpSources,
			)
		}
	}
	return nil
}

func (r *LabReconciler) materializeDevices(ctx context.Context, lab *laboratoryv1alpha1.Lab, resolvedInterfaces map[string][]laboratoryv1alpha1.InterfaceSpec) error {
	vniAllocator := poolpkg.NewAllocator(r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize)

	for _, tmpl := range lab.Spec.Devices {
		deviceName := fmt.Sprintf("%s-%s", lab.Name, tmpl.Name)
		var existing laboratoryv1alpha1.Device
		if err := r.Get(ctx, types.NamespacedName{Name: deviceName, Namespace: lab.Namespace}, &existing); err == nil {
			// For switch/hub devices, ensure VNI is written even if the status update failed on a previous reconcile.
			isSwitch := existing.Spec.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch ||
				existing.Spec.Type == laboratoryv1alpha1.DeviceTypeHub
			if !isSwitch || existing.Status.VNI != nil {
				continue
			}
			// Device exists but VNI was not written — allocate and write it now.
			vni, err := vniAllocator.AllocateIndex(ctx)
			if err != nil {
				return fmt.Errorf("allocate VNI for switch %s: %w", deviceName, err)
			}
			existing.Status.VNI = &vni
			if err := r.Status().Update(ctx, &existing); err != nil {
				return err
			}
			continue
		} else if !errors.IsNotFound(err) {
			return err
		}

		d := &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{
				Name:       deviceName,
				Namespace:  lab.Namespace,
				Labels:     deviceLabels(lab),
				Finalizers: []string{names.FinalizerOVSCleanup},
			},
			Spec: laboratoryv1alpha1.DeviceSpec{
				LabRef:         lab.Name,
				Name:           tmpl.Name,
				Type:           tmpl.Type,
				Image:          tmpl.Image,
				SecurityPreset: tmpl.SecurityPreset,
				Interfaces:     resolvedInterfaces[tmpl.Name],
				Exposure:       tmpl.Exposure,
				Resources:      tmpl.Resources,
				State:          r.deviceStateSpec(lab, tmpl.Type),
				ImageMirror:    r.deviceMirror(lab, tmpl.Type),
				ImageDigests:   r.deviceDigests(lab, tmpl),
			},
		}
		if err := controllerutil.SetOwnerReference(lab, d, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, d); err != nil {
			return err
		}

		if tmpl.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch || tmpl.Type == laboratoryv1alpha1.DeviceTypeHub {
			vni, err := vniAllocator.AllocateIndex(ctx)
			if err != nil {
				return fmt.Errorf("allocate VNI for switch %s: %w", deviceName, err)
			}
			d.Status.VNI = &vni
			if err := r.Status().Update(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// deviceLabels are the labels of a Device: the labels the caller put on its Lab (they
// reach the pod from here) and the lab's own key.
func deviceLabels(lab *laboratoryv1alpha1.Lab) map[string]string {
	labels := names.UserLabels(lab.Labels)
	labels[names.LabelLab] = lab.Name
	return labels
}

func isSwitchDevice(name string, lab *laboratoryv1alpha1.Lab) bool {
	for _, d := range lab.Spec.Devices {
		if d.Name == name {
			return d.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch || d.Type == laboratoryv1alpha1.DeviceTypeHub
		}
	}
	return false
}

func (r *LabReconciler) materializeConnections(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	vniAllocator := poolpkg.NewAllocator(r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize)

	for _, tmpl := range lab.Spec.Connections {
		connName := connectionName(lab.Name, tmpl.Endpoints)

		isDirect := true
		for _, ep := range tmpl.Endpoints {
			if isSwitchDevice(ep.Device, lab) {
				isDirect = false
				break
			}
		}

		var existing laboratoryv1alpha1.Connection
		if err := r.Get(ctx, types.NamespacedName{Name: connName, Namespace: lab.Namespace}, &existing); err == nil {
			// Repair: a direct connection whose VNI status write failed after
			// Create would otherwise stay VNI-less forever, and the node-agent
			// requeues indefinitely waiting for it.
			if isDirect && existing.Status.VNI == nil {
				vni, vniErr := vniAllocator.AllocateIndex(ctx)
				if vniErr != nil {
					return fmt.Errorf("allocate VNI for connection %s: %w", connName, vniErr)
				}
				existing.Status.VNI = &vni
				if err := r.Status().Update(ctx, &existing); err != nil {
					_ = vniAllocator.ReleaseIndex(ctx, vni)
					return err
				}
			}
			continue
		} else if !errors.IsNotFound(err) {
			return err
		}

		conn := &laboratoryv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{
				Name:       connName,
				Namespace:  lab.Namespace,
				Labels:     deviceLabels(lab),
				Finalizers: []string{names.FinalizerOVSCleanup},
			},
			Spec: laboratoryv1alpha1.ConnectionSpec{
				LabRef:    lab.Name,
				Endpoints: tmpl.Endpoints,
			},
		}
		if err := controllerutil.SetOwnerReference(lab, conn, r.Scheme); err != nil {
			return err
		}

		// Allocate VNI before Create; save it because Create() zeroes Status from server response.
		var allocatedVNI *uint
		if isDirect {
			vni, vniErr := vniAllocator.AllocateIndex(ctx)
			if vniErr != nil {
				return fmt.Errorf("allocate VNI for connection %s: %w", connName, vniErr)
			}
			allocatedVNI = &vni
		}

		if err := r.Create(ctx, conn); err != nil {
			// Return the VNI: the Connection does not exist, so nothing records
			// this index and the delete path would never release it.
			if allocatedVNI != nil {
				_ = vniAllocator.ReleaseIndex(ctx, *allocatedVNI)
			}
			return err
		}
		if allocatedVNI != nil {
			conn.Status.VNI = allocatedVNI
			if err := r.Status().Update(ctx, conn); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneConnections deletes Connection CRs owned by the lab whose names are no
// longer produced by lab.Spec.Connections, releasing any direct-link VNI first.
// This is what makes "remove a connection from the spec" tear down its OVS link
// (materializeConnections only ever creates).
func (r *LabReconciler) pruneConnections(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	desired := make(map[string]bool, len(lab.Spec.Connections))
	for i := range lab.Spec.Connections {
		desired[connectionName(lab.Name, lab.Spec.Connections[i].Endpoints)] = true
	}
	var list laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
		return err
	}
	vniAllocator := poolpkg.NewAllocator(r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize)
	for i := range list.Items {
		c := &list.Items[i]
		if desired[c.Name] || !c.DeletionTimestamp.IsZero() {
			continue
		}
		if c.Status.VNI != nil {
			if err := vniAllocator.ReleaseIndex(ctx, *c.Status.VNI); err != nil {
				return fmt.Errorf("release VNI %d for connection %s: %w", *c.Status.VNI, c.Name, err)
			}
			c.Status.VNI = nil
			if err := r.Status().Update(ctx, c); err != nil {
				return err
			}
		}
		if err := r.Delete(ctx, c); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// pruneDevices deletes Device CRs owned by the lab whose names are no longer in
// lab.Spec.Devices, releasing any switch/hub VNI first. This is what makes
// "remove a device from the spec" tear down its pod (materializeDevices only
// ever creates).
func (r *LabReconciler) pruneDevices(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	desired := make(map[string]bool, len(lab.Spec.Devices))
	for i := range lab.Spec.Devices {
		desired[fmt.Sprintf("%s-%s", lab.Name, lab.Spec.Devices[i].Name)] = true
	}
	var list laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
		return err
	}
	vniAllocator := poolpkg.NewAllocator(r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize)
	for i := range list.Items {
		d := &list.Items[i]
		if desired[d.Name] || !d.DeletionTimestamp.IsZero() {
			continue
		}
		if d.Status.VNI != nil {
			if err := vniAllocator.ReleaseIndex(ctx, *d.Status.VNI); err != nil {
				return fmt.Errorf("release switch VNI %d for device %s: %w", *d.Status.VNI, d.Name, err)
			}
			d.Status.VNI = nil
			if err := r.Status().Update(ctx, d); err != nil {
				return err
			}
		}
		if err := r.Delete(ctx, d); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// connectionName produces a deterministic, length-bounded Connection name from lab name + endpoints.
// Endpoints are sorted so ordering differences in the spec don't produce different names.
func connectionName(labName string, endpoints []laboratoryv1alpha1.EndpointSpec) string {
	parts := make([]string, 0, len(endpoints))
	unsafe := false
	for _, ep := range endpoints {
		if !dnsSafeNamePart(ep.Device) || !dnsSafeNamePart(ep.Interface) {
			unsafe = true
		}
		if ep.Interface != "" {
			parts = append(parts, ep.Device+"-"+ep.Interface)
		} else {
			parts = append(parts, ep.Device)
		}
	}
	sort.Strings(parts)
	if unsafe {
		identity := make([]string, 0, len(endpoints))
		for _, ep := range endpoints {
			identity = append(identity, fmt.Sprintf("%d:%s%d:%s", len(ep.Device), ep.Device, len(ep.Interface), ep.Interface))
		}
		sort.Strings(identity)
		sum := sha256.Sum256([]byte(strings.Join(identity, "|")))
		prefix := labName
		if len(prefix) > 220 {
			prefix = strings.TrimRight(prefix[:220], "-")
		}
		return prefix + "--" + hex.EncodeToString(sum[:12])
	}
	full := labName + "--" + strings.Join(parts, "--")
	if len(full) <= 253 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(sum[:4])
	prefix := strings.TrimRight(full[:243], "-")
	return prefix + "--" + suffix
}

func dnsSafeNamePart(value string) bool {
	for _, ch := range value {
		if ch != '-' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

func (r *LabReconciler) updateStatus(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	var deviceList laboratoryv1alpha1.DeviceList
	if err := r.List(
		ctx, &deviceList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{names.LabelLab: lab.Name},
	); err != nil {
		return ctrl.Result{}, err
	}

	var refs []laboratoryv1alpha1.DeviceRef
	allReady := len(deviceList.Items) > 0
	for _, d := range deviceList.Items {
		refs = append(refs, laboratoryv1alpha1.DeviceRef{Name: d.Spec.Name, Ready: d.Status.Ready, State: deviceStateInfo(&d)})
		if !d.Status.Ready {
			allReady = false
		}
	}

	throttled := throttleStateInfo(lab.Status.Devices, refs)

	var connList laboratoryv1alpha1.ConnectionList
	if err := r.List(
		ctx, &connList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{names.LabelLab: lab.Name},
	); err != nil {
		return ctrl.Result{}, err
	}

	var connRefs []laboratoryv1alpha1.ConnectionRef
	for _, c := range connList.Items {
		connRefs = append(connRefs, laboratoryv1alpha1.ConnectionRef{Name: c.Name, Ready: c.Status.Ready})
		if !c.Status.Ready {
			allReady = false
		}
	}

	// The VPN and Internet segments are ready once their LabVPN / LabGateway
	// report Ready. The VPN pod admits client traffic to a lab only while
	// Status.VPN.Ready is true, so this must be kept in sync.
	vpnReady, err := r.segmentReady(ctx, lab.Namespace, lab.Spec.VPN.Enabled, names.LabVPNObjectName(lab.Name), &laboratoryv1alpha1.LabVPN{})
	if err != nil {
		return ctrl.Result{}, err
	}
	inetReady, err := r.segmentReady(ctx, lab.Namespace, lab.Spec.Internet.Enabled, names.LabGatewayObjectName(lab.Name), &laboratoryv1alpha1.LabGateway{})
	if err != nil {
		return ctrl.Result{}, err
	}
	if (lab.Spec.VPN.Enabled && !vpnReady) || (lab.Spec.Internet.Enabled && !inetReady) {
		allReady = false
	}

	newPhase := laboratoryv1alpha1.PhaseProvisioning
	if allReady {
		newPhase = laboratoryv1alpha1.PhaseReady
	}

	access := r.buildAccessEntries(ctx, lab)

	// Reflect readiness as a condition; on the Ready edge emit a Normal event.
	wasReady := labstatus.IsReady(lab.Status.Conditions)
	if allReady {
		labstatus.SetReady(
			&lab.Status.Conditions,
			lab.Generation,
			true,
			labstatus.ReasonReady,
			"all devices and connections ready",
		)
		if !wasReady {
			r.Recorder.Event(lab, corev1.EventTypeNormal, labstatus.ReasonReady, "lab is ready")
		}
	} else {
		labstatus.SetReady(
			&lab.Status.Conditions,
			lab.Generation,
			false,
			labstatus.ReasonProvisioning,
			"waiting for devices and connections to become ready",
		)
	}

	if newPhase == lab.Status.Phase &&
		vpnReady == lab.Status.VPN.Ready &&
		inetReady == lab.Status.Internet.Ready &&
		reflect.DeepEqual(refs, lab.Status.Devices) &&
		reflect.DeepEqual(connRefs, lab.Status.Connections) &&
		reflect.DeepEqual(access, lab.Status.Access) &&
		wasReady == allReady {
		if newPhase != laboratoryv1alpha1.PhaseReady {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if throttled {
			return ctrl.Result{RequeueAfter: snapshotInfoInterval}, nil
		}
		return ctrl.Result{}, nil
	}

	lab.Status.VPN.Ready = vpnReady
	lab.Status.Internet.Ready = inetReady
	lab.Status.Devices = refs
	lab.Status.Connections = connRefs
	lab.Status.Phase = newPhase
	lab.Status.Access = access

	if err := r.Status().Update(ctx, lab); err != nil {
		return ctrl.Result{}, err
	}
	if newPhase != laboratoryv1alpha1.PhaseReady {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// segmentReady reports whether the lab's LabVPN / LabGateway object is Ready.
// A disabled segment is never ready; a missing object is not ready yet.
func (r *LabReconciler) segmentReady(ctx context.Context, ns string, enabled bool, name string, obj client.Object) (bool, error) {
	if !enabled {
		return false, nil
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	switch o := obj.(type) {
	case *laboratoryv1alpha1.LabVPN:
		return labstatus.IsReady(o.Status.Conditions), nil
	case *laboratoryv1alpha1.LabGateway:
		return labstatus.IsReady(o.Status.Conditions), nil
	}
	return false, nil
}

// buildAccessEntries returns the externally-visible URL for each web-exposed
// device in the lab. Empty if BaseDomain is unset.
// The host is the name of the device's existing web Service, never recomputed;
// a device whose Service does not exist yet is skipped.
func (r *LabReconciler) buildAccessEntries(ctx context.Context, lab *laboratoryv1alpha1.Lab) []laboratoryv1alpha1.AccessEntry {
	if r.BaseDomain == "" {
		return nil
	}
	var out []laboratoryv1alpha1.AccessEntry
	for _, d := range lab.Spec.Devices {
		if d.Exposure == nil || d.Exposure.Web == nil {
			continue
		}
		proto := d.Exposure.Web.Protocol
		if proto == "" {
			proto = "http"
		}
		svc, err := r.findWebService(ctx, lab, d.Name)
		if err != nil || svc == nil {
			continue
		}
		out = append(
			out, laboratoryv1alpha1.AccessEntry{
				Device:   d.Name,
				Port:     d.Exposure.Web.Port,
				Protocol: proto,
				URL:      fmt.Sprintf("https://%s.%s", svc.Name, r.BaseDomain),
			},
		)
	}
	return out
}

func (r *LabReconciler) reconcileDelete(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var deviceList laboratoryv1alpha1.DeviceList
	if err := r.List(
		ctx, &deviceList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{names.LabelLab: lab.Name},
	); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("reconcileDelete: listed devices", "count", len(deviceList.Items))

	// Delete connections and devices concurrently. Connections must be deleted
	// alongside devices (not after) to avoid a deadlock: DevicePortReconciler
	// waits for Connection OVS-cleanup finalizers before removing the Device
	// finalizer, but connections are only deleted by this function.
	vniAllocator := poolpkg.NewAllocator(r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize)

	var connList laboratoryv1alpha1.ConnectionList
	if err := r.List(
		ctx, &connList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{names.LabelLab: lab.Name},
	); err != nil {
		return ctrl.Result{}, err
	}
	for i := range connList.Items {
		c := &connList.Items[i]
		if c.Status.VNI != nil {
			if err := vniAllocator.ReleaseIndex(ctx, *c.Status.VNI); err != nil {
				logger.Error(err, "release VNI", "connection", c.Name, "vni", *c.Status.VNI)
			}
			c.Status.VNI = nil
			if err := r.Status().Update(ctx, c); err != nil {
				return ctrl.Result{}, err
			}
		}
		if c.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, c); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	}

	deletedAny := false
	for i := range deviceList.Items {
		d := &deviceList.Items[i]
		if d.Status.VNI != nil {
			if err := vniAllocator.ReleaseIndex(ctx, *d.Status.VNI); err != nil {
				return ctrl.Result{}, fmt.Errorf("release switch VNI %d for device %s: %w", *d.Status.VNI, d.Name, err)
			}
			d.Status.VNI = nil
			if err := r.Status().Update(ctx, d); err != nil {
				return ctrl.Result{}, err
			}
		}
		if d.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, d); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			deletedAny = true
		}
	}

	if len(deviceList.Items) > 0 || deletedAny || len(connList.Items) > 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Remove annotation entries so node-agent stops maintaining the veths.
	if lab.Status.VPN.CIDR != "" {
		if n, ok := indexFromCIDR(lab.Status.VPN.CIDR); ok {
			_ = r.patchDeploymentNetworks(ctx, lab.Namespace, "vpn", names.LabIfaceNameByIndex(n), names.VPNHostPortKey(lab.Namespace, n), false)
		}
	}
	if lab.Status.Internet.CIDR != "" {
		if n, ok := indexFromCIDR(lab.Status.Internet.CIDR); ok {
			_ = r.patchDeploymentNetworks(
				ctx, lab.Namespace, "gateway",
				names.LabIfaceNameByIndex(n), names.GWHostPortKey(lab.Namespace, n), false,
			)
		}
	}

	// Delete LabVPN and wait for VPN binary to complete cleanup.
	if done, err := r.ensureLabVPNDeleted(ctx, lab); err != nil {
		return ctrl.Result{}, err
	} else if !done {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Delete LabGateway and wait for gateway binary to complete cleanup.
	if done, err := r.ensureLabGatewayDeleted(ctx, lab); err != nil {
		return ctrl.Result{}, err
	} else if !done {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Release lab subnet index if allocated.
	if lab.Status.VPN.CIDR != "" || lab.Status.Internet.CIDR != "" {
		cidr := lab.Status.VPN.CIDR
		if cidr == "" {
			cidr = lab.Status.Internet.CIDR
		}
		if n, ok := indexFromCIDR(cidr); ok {
			subnetAllocator := poolpkg.NewAllocator(r.Client, names.PoolLabSubnets, lab.Namespace, 254)
			if releaseErr := subnetAllocator.ReleaseIndex(ctx, n); releaseErr != nil {
				logger.Error(releaseErr, "release lab subnet", "n", n)
			}
		}
	}

	controllerutil.RemoveFinalizer(lab, names.FinalizerLab)
	return ctrl.Result{}, r.Update(ctx, lab)
}

// ensureSubnetAllocation allocates a /24 index N from the lab-subnets pool,
// writes Status.VPN.CIDR and/or Status.Internet.CIDR, and updates status.
// VPN and Internet share the same N for a given Lab.
func (r *LabReconciler) ensureSubnetAllocation(ctx context.Context, lab *laboratoryv1alpha1.Lab) (
	updated bool,
	err error,
) {
	needsVPN := lab.Spec.VPN.Enabled && lab.Status.VPN.CIDR == ""
	needsInet := lab.Spec.Internet.Enabled && lab.Status.Internet.CIDR == ""
	if !needsVPN && !needsInet {
		return false, nil
	}

	subnetAllocator := poolpkg.NewAllocator(r.Client, names.PoolLabSubnets, lab.Namespace, 254)
	n, err := subnetAllocator.AllocateIndex(ctx)
	if err != nil {
		return false, fmt.Errorf("allocate lab subnet: %w", err)
	}
	if needsVPN {
		cidr, err := netutil.SubnetForIndex(r.VPNBaseNetwork, 24, n)
		if err != nil {
			return false, fmt.Errorf("compute VPN subnet: %w", err)
		}
		lab.Status.VPN.CIDR = cidr
	}
	if needsInet {
		cidr, err := netutil.SubnetForIndex(r.InetBaseNetwork, 24, n)
		if err != nil {
			return false, fmt.Errorf("compute inet subnet: %w", err)
		}
		lab.Status.Internet.CIDR = cidr
	}
	return true, r.Status().Update(ctx, lab)
}

// indexFromCIDR extracts the subnet index N from a CIDR like "10.X.N.0/24".
func indexFromCIDR(cidr string) (uint, bool) {
	parts := strings.Split(cidr, ".")
	if len(parts) < 3 {
		return 0, false
	}
	var n uint
	_, err := fmt.Sscanf(parts[2], "%d", &n)
	return n, err == nil
}

// ensureLabNetworkObjects creates LabVPN and/or LabGateway CRDs once the subnet
// index is known. The FinalizerController on each object signals VPN/gateway
// binaries that the spec is complete and they may start reconciling.
func (r *LabReconciler) ensureLabNetworkObjects(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	if lab.Spec.VPN.Enabled && lab.Status.VPN.CIDR != "" {
		n, ok := indexFromCIDR(lab.Status.VPN.CIDR)
		if !ok {
			return fmt.Errorf("invalid VPN CIDR %q", lab.Status.VPN.CIDR)
		}
		if err := r.ensureLabVPN(ctx, lab, n); err != nil {
			return err
		}
		if err := r.ensureDHCPPool(ctx, lab, "dhcp-vpn"); err != nil {
			return err
		}
	}
	if lab.Spec.Internet.Enabled && lab.Status.Internet.CIDR != "" {
		n, ok := indexFromCIDR(lab.Status.Internet.CIDR)
		if !ok {
			return fmt.Errorf("invalid Internet CIDR %q", lab.Status.Internet.CIDR)
		}
		if err := r.ensureLabGateway(ctx, lab, n); err != nil {
			return err
		}
		if err := r.ensureDHCPPool(ctx, lab, "dhcp-inet"); err != nil {
			return err
		}
	}
	return nil
}

// ensureDHCPPool creates the per-lab DHCP Pool that the VPN/gateway binaries
// use as the enable-signal (and future lease allocator) for their embedded
// DHCP server. Without this Pool the binaries silently never start DHCP.
// prefix is "dhcp-vpn" or "dhcp-inet"; the segment's DHCPServer.Enabled
// gates creation. Owner reference ties the Pool's lifecycle to the Lab.
func (r *LabReconciler) ensureDHCPPool(ctx context.Context, lab *laboratoryv1alpha1.Lab, prefix string) error {
	var dhcpSpec *laboratoryv1alpha1.DHCPServer
	switch prefix {
	case "dhcp-vpn":
		dhcpSpec = lab.Spec.VPN.DHCPServer
	case "dhcp-inet":
		dhcpSpec = lab.Spec.Internet.DHCPServer
	}
	if dhcpSpec == nil || !dhcpSpec.Enabled {
		return nil
	}

	poolName := fmt.Sprintf("%s-%s-0", prefix, lab.Name)
	var existing allocationv1alpha1.Pool
	if err := r.Get(ctx, types.NamespacedName{Name: poolName, Namespace: lab.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Leases live in hosts .2‥.254 of the lab /24 — .1 is the gateway/VPN leg.
	const dhcpPoolOffset, dhcpPoolSize = 2, 253
	bitmapStr, free := poolpkg.InitBitmap(dhcpPoolSize, dhcpPoolOffset)
	p := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: lab.Namespace,
			Labels: map[string]string{
				poolpkg.PoolTypeLabel:   prefix,
				poolpkg.PoolStateLabel:  poolpkg.PoolStateEmpty,
				poolpkg.PoolGroupLabel:  fmt.Sprintf("%s-%s", prefix, lab.Name),
				poolpkg.LatestPoolLabel: "true",
			},
		},
		Spec: allocationv1alpha1.PoolSpec{Size: dhcpPoolSize, Offset: dhcpPoolOffset},
	}
	if err := controllerutil.SetOwnerReference(lab, p, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, p); err != nil {
		return err
	}
	p.Status = allocationv1alpha1.PoolStatus{Free: free, BitMap: bitmapStr}
	return r.Status().Update(ctx, p)
}

func (r *LabReconciler) ensureLabVPN(ctx context.Context, lab *laboratoryv1alpha1.Lab, n uint) error {
	name := names.LabVPNObjectName(lab.Name)
	var existing laboratoryv1alpha1.LabVPN
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	obj := &laboratoryv1alpha1.LabVPN{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  lab.Namespace,
			Finalizers: []string{names.FinalizerController},
		},
		Spec: laboratoryv1alpha1.LabVPNSpec{
			LabName:      lab.Name,
			NetworkIndex: n,
		},
	}
	if err := controllerutil.SetOwnerReference(lab, obj, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, obj)
}

func (r *LabReconciler) ensureLabGateway(ctx context.Context, lab *laboratoryv1alpha1.Lab, n uint) error {
	name := names.LabGatewayObjectName(lab.Name)
	var existing laboratoryv1alpha1.LabGateway
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	obj := &laboratoryv1alpha1.LabGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  lab.Namespace,
			Finalizers: []string{names.FinalizerController},
		},
		Spec: laboratoryv1alpha1.LabGatewaySpec{
			LabName:      lab.Name,
			NetworkIndex: n,
		},
	}
	if err := controllerutil.SetOwnerReference(lab, obj, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, obj)
}

// ensureLabVPNDeleted triggers deletion of the LabVPN object and, once the VPN
// binary has removed its own finalizer (leaving only FinalizerController), removes
// FinalizerController so the object can be garbage-collected.
// Returns true when the object no longer exists.
func (r *LabReconciler) ensureLabVPNDeleted(ctx context.Context, lab *laboratoryv1alpha1.Lab) (bool, error) {
	if !lab.Spec.VPN.Enabled {
		return true, nil
	}
	name := names.LabVPNObjectName(lab.Name)
	var obj laboratoryv1alpha1.LabVPN
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &obj); err != nil {
		return true, client.IgnoreNotFound(err)
	}
	if obj.DeletionTimestamp.IsZero() {
		return false, r.Delete(ctx, &obj)
	}
	// The VPN binary removes its own finalizer after in-pod cleanup, leaving
	// only FinalizerController for us to strip. The VPN pod is a Deployment, so
	// if it crashes a new one comes up and processes this — the only way it
	// never runs is a group teardown, which LabGroup.reconcileDelete prevents by
	// draining Labs before the namespace (and its VPN Deployment) is deleted.
	f := obj.GetFinalizers()
	if len(f) == 1 && f[0] == names.FinalizerController {
		controllerutil.RemoveFinalizer(&obj, names.FinalizerController)
		return false, r.Update(ctx, &obj)
	}
	return false, nil
}

// ensureLabGatewayDeleted is ensureLabVPNDeleted's counterpart for LabGateway.
func (r *LabReconciler) ensureLabGatewayDeleted(ctx context.Context, lab *laboratoryv1alpha1.Lab) (bool, error) {
	if !lab.Spec.Internet.Enabled {
		return true, nil
	}
	name := names.LabGatewayObjectName(lab.Name)
	var obj laboratoryv1alpha1.LabGateway
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: lab.Namespace}, &obj); err != nil {
		return true, client.IgnoreNotFound(err)
	}
	if obj.DeletionTimestamp.IsZero() {
		return false, r.Delete(ctx, &obj)
	}
	f := obj.GetFinalizers()
	if len(f) == 1 && f[0] == names.FinalizerController {
		controllerutil.RemoveFinalizer(&obj, names.FinalizerController)
		return false, r.Update(ctx, &obj)
	}
	return false, nil
}

// findWebService returns the web Service of a device of the lab: the one with
// the lab and device labels that the lab owns. Nothing else stores the name, so
// this is how later reconciles keep the host label stable. nil if none exists.
func (r *LabReconciler) findWebService(ctx context.Context, lab *laboratoryv1alpha1.Lab, device string) (*corev1.Service, error) {
	var list corev1.ServiceList
	if err := r.List(
		ctx, &list, client.InNamespace(lab.Namespace),
		client.MatchingLabels{names.LabelLab: lab.Name, names.LabelDevice: device},
	); err != nil {
		return nil, err
	}
	var found *corev1.Service
	for i := range list.Items {
		svc := &list.Items[i]
		if !ownedByLab(svc, lab) {
			continue
		}
		// Duplicates are not expected; stay deterministic if one ever appears.
		if found == nil || svc.CreationTimestamp.Before(&found.CreationTimestamp) ||
			(svc.CreationTimestamp.Equal(&found.CreationTimestamp) && svc.Name < found.Name) {
			found = svc
		}
	}
	return found, nil
}

func ownedByLab(obj metav1.Object, lab *laboratoryv1alpha1.Lab) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == lab.UID && ref.Kind == "Lab" {
			return true
		}
	}
	return false
}

// createWebService creates the device's Service under a fresh random host
// label. Kubernetes settles uniqueness in the group namespace: on AlreadyExists
// another code is drawn, WebCodeAttempts times at WebCodeLen and then
// WebCodeAttempts more at WebCodeMaxLen, never longer.
func (r *LabReconciler) createWebService(ctx context.Context, lab *laboratoryv1alpha1.Lab, device string, fill func(*corev1.Service) error) (*corev1.Service, error) {
	for attempt := 0; attempt < 2*names.WebCodeAttempts; attempt++ {
		n := names.WebCodeLen
		if attempt >= names.WebCodeAttempts {
			n = names.WebCodeMaxLen
		}
		draw := r.newWebCode
		if draw == nil {
			draw = names.NewWebCode
		}
		code, err := draw(n)
		if err != nil {
			return nil, err
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: names.WebHostLabel(device, code), Namespace: lab.Namespace},
		}
		if err := fill(svc); err != nil {
			return nil, err
		}
		err = r.Create(ctx, svc)
		if err == nil {
			return svc, nil
		}
		if !errors.IsAlreadyExists(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free web host label for device %s", device)
}

func (r *LabReconciler) ensureWebServices(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	for _, d := range lab.Spec.Devices {
		if d.Exposure == nil || d.Exposure.Web == nil {
			continue
		}
		web := d.Exposure.Web
		// The Service name is the host label <device>-<code>. The labels let the
		// proxy attribute a request to the lab and the device.
		fill := func(svc *corev1.Service) error {
			if svc.Labels == nil {
				svc.Labels = map[string]string{}
			}
			svc.Labels[names.LabelLab] = lab.Name
			svc.Labels[names.LabelDevice] = d.Name
			svc.Spec.Selector = map[string]string{
				names.LabelLab: lab.Name,
				"app":          d.Name,
			}
			protocol := web.Protocol
			if protocol == "" {
				protocol = "http"
			}
			svc.Spec.Ports = []corev1.ServicePort{
				{
					Name:       protocol,
					Port:       web.Port,
					TargetPort: intstr.FromInt32(web.Port),
					Protocol:   corev1.ProtocolTCP,
				},
			}
			svc.Spec.Type = corev1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = "None" // headless: DNS returns pod IP directly
			return controllerutil.SetOwnerReference(lab, svc, r.Scheme)
		}

		existing, err := r.findWebService(ctx, lab, d.Name)
		if err != nil {
			return fmt.Errorf("find Service of device %s: %w", d.Name, err)
		}
		var svc *corev1.Service
		if existing == nil {
			svc, err = r.createWebService(ctx, lab, d.Name, fill)
		} else {
			svc = existing
			_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error { return fill(svc) })
		}
		if err != nil {
			return fmt.Errorf("ensure Service of device %s: %w", d.Name, err)
		}
		svcName := svc.Name

		np := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: svcName + "-web", Namespace: lab.Namespace},
		}
		_, err = controllerutil.CreateOrUpdate(
			ctx, r.Client, np, func() error {
				// Allow ingress from the proxy pod identified by namespace+label.
				peers := []networkingv1.NetworkPolicyPeer{
					{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": names.ProxyNamespace},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": names.ProxyL7App},
						},
					},
				}
				// When the proxy uses hostNetwork its source IP is the node IP, not a pod
				// IP, so the namespace/label selector above is ineffective for that traffic.
				// Add explicit ipBlock peers for each configured CIDR.
				for _, cidr := range r.ProxySourceCIDRs {
					peers = append(
						peers, networkingv1.NetworkPolicyPeer{
							IPBlock: &networkingv1.IPBlock{CIDR: cidr},
						},
					)
				}
				np.Spec = networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": d.Name}},
					PolicyTypes: []networkingv1.PolicyType{
						networkingv1.PolicyTypeIngress,
						networkingv1.PolicyTypeEgress,
					},
					Ingress: []networkingv1.NetworkPolicyIngressRule{
						{
							From: peers,
							Ports: []networkingv1.NetworkPolicyPort{
								{
									Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: web.Port},
									Protocol: func() *corev1.Protocol { p := corev1.ProtocolTCP; return &p }(),
								},
							},
						},
					},
					Egress: []networkingv1.NetworkPolicyEgressRule{},
				}
				return controllerutil.SetOwnerReference(lab, np, r.Scheme)
			},
		)
		if err != nil {
			return fmt.Errorf("ensure NetworkPolicy %s: %w", svcName, err)
		}
	}
	return nil
}

// ensureDeploymentAnnotations adds the lab's OVS interface entries to the VPN and/or
// gateway Deployment pod-template annotation so node-agent attaches them.
// The annotation entry format is "lab{N}@{ovsPortName}" so node-agent creates a
// veth with ovsPortName (VPNHostPortKey / GWHostPortKey) as the OVS port and
// renames the pod-side to lab{N}.
func (r *LabReconciler) ensureDeploymentAnnotations(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	if lab.Spec.VPN.Enabled && lab.Status.VPN.CIDR != "" {
		n, ok := indexFromCIDR(lab.Status.VPN.CIDR)
		if ok {
			if err := r.patchDeploymentNetworks(ctx, lab.Namespace, "vpn", names.LabIfaceNameByIndex(n), names.VPNHostPortKey(lab.Namespace, n), true); err != nil {
				return err
			}
		}
	}
	if lab.Spec.Internet.Enabled && lab.Status.Internet.CIDR != "" {
		n, ok := indexFromCIDR(lab.Status.Internet.CIDR)
		if ok {
			// Pod-side iface is lab{N}; the host-side OVS port is per group and
			// per leg, so it collides neither with the VPN leg nor with other groups.
			if err := r.patchDeploymentNetworks(
				ctx, lab.Namespace, "gateway",
				names.LabIfaceNameByIndex(n), names.GWHostPortKey(lab.Namespace, n), true,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// patchDeploymentNetworks adds or removes a "{podIfaceName}@{ovsPortName}" entry from
// the network.cybericebox.com/networks annotation on a Deployment pod template.
// node-agent creates a veth whose host side is registered in OVS as ovsPortName and
// whose pod side is moved into the pod netns and renamed to podIfaceName.
func (r *LabReconciler) patchDeploymentNetworks(
	ctx context.Context,
	ns, deployName, podIfaceName, ovsPortName string,
	add bool,
) error {
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: deployName, Namespace: ns}, &dep); err != nil {
		return client.IgnoreNotFound(err)
	}

	entry := podIfaceName + "@" + ovsPortName
	original := dep.DeepCopy()

	ann := dep.Spec.Template.Annotations[names.AnnotationNetworks]
	var entries []string
	for _, e := range strings.Split(ann, ",") {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}

	if add {
		for _, e := range entries {
			if e == entry {
				return nil // already present
			}
		}
		entries = append(entries, entry)
	} else {
		filtered := entries[:0]
		for _, e := range entries {
			if e != entry {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == len(entries) {
			return nil // not present, nothing to do
		}
		entries = filtered
	}

	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[names.AnnotationNetworks] = strings.Join(entries, ",")
	return r.Patch(ctx, &dep, client.MergeFrom(original))
}

func (r *LabReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Lab{}).
		Owns(&laboratoryv1alpha1.Device{}).
		Owns(&laboratoryv1alpha1.Connection{}).
		Owns(&laboratoryv1alpha1.LabVPN{}).
		Owns(&laboratoryv1alpha1.LabGateway{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
