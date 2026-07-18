package laboratory

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// DeviceReconciler reconciles a Device object.
type DeviceReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	LabNodeSelector map[string]string
	LabTolerations  []corev1.Toleration
	// NetConfigImage is the image used for the optional init-container that
	// assigns static IP/routes inside a device pod. Must contain `ip` (iproute2)
	// and `sh`. Empty disables static addressing via init-container.
	NetConfigImage string
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=connections,verbs=get;list;watch

func (r *DeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var device laboratoryv1alpha1.Device
	if err := r.Get(ctx, req.NamespacedName, &device); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !device.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&device, names.FinalizerOVSCleanup) {
			// node-agent removes this finalizer after OVS cleanup; poll.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	switch device.Spec.Type {
	case laboratoryv1alpha1.DeviceTypeUnmanagedSwitch, laboratoryv1alpha1.DeviceTypeHub:
		return r.reconcileSwitch(ctx, &device)
	}

	return r.reconcileWorkload(ctx, &device)
}

// reconcileSwitch resolves readiness for a switch/hub device, which runs no pod.
// A switch is a broadcast domain: it is Ready only once its VNI is allocated AND
// every Connection that attaches to it is wired (Ready) — with at least one such
// Connection. An unwired switch (no Connection selected yet, or a link still
// coming up) is NOT Ready, so the lab does not report a half-built fabric as up.
func (r *DeviceReconciler) reconcileSwitch(ctx context.Context, device *laboratoryv1alpha1.Device) (ctrl.Result, error) {
	ready, err := r.switchReady(ctx, device)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ready != device.Status.Ready {
		device.Status.Ready = ready
		if err := r.Status().Update(ctx, device); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Connection changes enqueue this device (Watches), but requeue as a fallback
	// while not ready in case a status write on the connection side is missed.
	if !ready {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// switchReady reports whether a switch/hub's fabric is fully established: its VNI
// is allocated and every Connection in the lab that references it is Ready. False
// when no Connection references it yet (nothing to be ready for).
func (r *DeviceReconciler) switchReady(ctx context.Context, device *laboratoryv1alpha1.Device) (bool, error) {
	if device.Status.VNI == nil {
		return false, nil
	}
	var conns laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &conns, client.InNamespace(device.Namespace)); err != nil {
		return false, err
	}
	found := false
	for i := range conns.Items {
		c := &conns.Items[i]
		if c.Spec.LabRef != device.Spec.LabRef || !connectionRefsDevice(c, device.Spec.Name) {
			continue
		}
		found = true
		if !c.Status.Ready {
			return false, nil
		}
	}
	return found, nil
}

// connectionRefsDevice reports whether a Connection has the named device as one
// of its endpoints.
func connectionRefsDevice(c *laboratoryv1alpha1.Connection, deviceName string) bool {
	for _, e := range c.Spec.Endpoints {
		if e.Device == deviceName {
			return true
		}
	}
	return false
}

// reconcileWorkload ensures the device's Deployment exists (replicas=1, Recreate
// strategy) and mirrors its readiness into the Device status. The ReplicaSet
// keeps exactly one pod alive — recreating it on node loss/eviction — so the
// device survives a pod death without the controller re-creating pods by hand.
// NodeName/PodIP come from the live pod (looked up by label), since a Deployment
// pod's name is non-deterministic.
func (r *DeviceReconciler) reconcileWorkload(ctx context.Context, device *laboratoryv1alpha1.Device) (ctrl.Result, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: device.Name, Namespace: device.Namespace}, &dep)

	if errors.IsNotFound(err) {
		return ctrl.Result{}, r.createDeployment(ctx, device)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	nodeName, podIP := r.devicePodPlacement(ctx, device)
	ready := dep.Status.AvailableReplicas >= 1

	updated := false
	if nodeName != device.Status.NodeName {
		device.Status.NodeName = nodeName
		updated = true
	}
	if podIP != device.Status.PodIP {
		device.Status.PodIP = podIP
		updated = true
	}
	if ready != device.Status.Ready {
		device.Status.Ready = ready
		updated = true
	}
	if updated {
		if err := r.Status().Update(ctx, device); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Deployment changes trigger reconcile, but a pod getting its IP does not
	// (the pod is owned by the ReplicaSet, not the Device) — requeue until the
	// placement is fully observed.
	if !ready || podIP == "" {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// devicePodPlacement returns the NodeName/PodIP of the device's live pod, found
// by label (the Deployment pod name carries a random suffix). On a brief overlap
// during recreation it takes the last Running match (the newest pod).
func (r *DeviceReconciler) devicePodPlacement(ctx context.Context, device *laboratoryv1alpha1.Device) (nodeName, podIP string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(device.Namespace),
		client.MatchingLabels{
			names.LabelLab:    device.Spec.LabRef,
			names.LabelDevice: device.Spec.Name,
		},
	); err != nil {
		return "", ""
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodRunning {
			nodeName, podIP = p.Spec.NodeName, p.Status.PodIP
		}
	}
	return nodeName, podIP
}

// deviceNetworkAnnotation builds the network.cybericebox.com/networks annotation value.
// Lists OVS attachments only; default k8s network is controlled by AnnotationDefaultNetwork.
// Format per entry: "iface@[connection][|MAC]"
// At pod creation time we don't know the Connection name yet, so entries are "iface@" or "iface@|MAC".
func deviceNetworkAnnotation(device *laboratoryv1alpha1.Device) string {
	var entries []string
	for _, iface := range device.Spec.Interfaces {
		entry := iface.Name + "@"
		if iface.MAC != "" {
			entry += "|" + iface.MAC
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ",")
}

func (r *DeviceReconciler) createDeployment(ctx context.Context, device *laboratoryv1alpha1.Device) error {
	annotations := map[string]string{
		names.AnnotationDevice:   device.Spec.Name,
		names.AnnotationNetworks: deviceNetworkAnnotation(device),
	}
	if device.Spec.Exposure != nil {
		annotations[names.AnnotationDefaultNetwork] = names.AccessPortIface
	} else if len(device.Spec.Interfaces) > 0 {
		annotations[names.AnnotationDefaultNetwork] = ""
	}

	labels := map[string]string{
		names.LabelLab:        device.Spec.LabRef,
		"app":                 device.Spec.Name,
		names.LabelDevice:     device.Spec.Name,
		names.LabelDeviceName: device.Name,
	}
	// The selector must be immutable and uniquely identify this device's pod:
	// (lab, device-name) is unique within the namespace.
	selectorLabels := map[string]string{
		names.LabelLab:    device.Spec.LabRef,
		names.LabelDevice: device.Spec.Name,
	}

	podSpec := corev1.PodSpec{
		NodeSelector: r.LabNodeSelector,
		Tolerations:  r.LabTolerations,
		Containers: []corev1.Container{
			{
				Name:  device.Spec.Name,
				Image: device.Spec.Image,
				// Run the image as-is (no entrypoint override). Capabilities are
				// opt-in: an image that only serves a port gets none; one that
				// runs networking/testing tools or an in-image DHCP client gets
				// the curated set it requested (plus NET_ADMIN+NET_RAW when it
				// has a DHCP interface). Isolation is enforced host-side by OVS
				// flows, so these caps cannot break a pod out of its VNI.
				SecurityContext: deviceSecurityContext(device),
				Resources:       deviceResources(device),
				// Env vars come from a per-device Secret (<device>-env) the agent
				// wrote write-only — referenced here, never read by the controller
				// (the kubelet resolves envFrom at pod start). optional=true so a
				// device with no env simply has no secret; values never live in the CR.
				EnvFrom: []corev1.EnvFromSource{
					{SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: device.Name + "-env"},
						Optional:             ptrBool(true),
					}},
				},
			},
		},
	}
	// Optional init-container: address static and dhcp-preset interfaces inside
	// the pod netns (node-agent only wires the L2 veth), so those device pods
	// need no capabilities of their own. addr.type=dhcp interfaces are handled by
	// the image's own client instead. Skipped when neither mode is present.
	if ic := r.netConfigInitContainer(device); ic != nil {
		podSpec.InitContainers = append(podSpec.InitContainers, *ic)
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      device.Name,
			Namespace: device.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrInt32(1),
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels},
			// Recreate, never RollingUpdate: a device is a single L2 identity with
			// one OVS port — two pods cannot share it. The old pod (held by the OVS
			// finalizer until node-agent cleanup) is fully torn down before the new
			// one is scheduled, so the port is free to re-attach to the replacement.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: podSpec,
			},
		},
	}

	if err := controllerutil.SetControllerReference(device, dep, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, dep)
}

func ptrBool(b bool) *bool    { return &b }
func ptrInt32(i int32) *int32 { return &i }

// deviceResources builds container resource requirements from the device's
// optional Resources spec. Empty or unparseable quantity strings are skipped,
// so a device with no (or partial) resources set is best-effort scheduled.
func deviceResources(device *laboratoryv1alpha1.Device) corev1.ResourceRequirements {
	var rr corev1.ResourceRequirements
	r := device.Spec.Resources
	if r == nil {
		return rr
	}
	set := func(list *corev1.ResourceList, name corev1.ResourceName, val string) {
		if val == "" {
			return
		}
		q, err := resource.ParseQuantity(val)
		if err != nil {
			return
		}
		if *list == nil {
			*list = corev1.ResourceList{}
		}
		(*list)[name] = q
	}
	set(&rr.Requests, corev1.ResourceCPU, r.CPURequest)
	set(&rr.Requests, corev1.ResourceMemory, r.MemoryRequest)
	set(&rr.Limits, corev1.ResourceCPU, r.CPULimit)
	set(&rr.Limits, corev1.ResourceMemory, r.MemoryLimit)
	return rr
}

// ifaceNameRE matches a valid Linux interface name (IFNAMSIZ-bounded, no shell
// metacharacters) so it is safe to interpolate into the init-container script.
var ifaceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)

func validIfaceName(name string) (string, bool) {
	if ifaceNameRE.MatchString(name) {
		return name, true
	}
	return "", false
}

// canonicalCIDR parses a CIDR and returns its canonical "ip/prefix" form, which
// by construction contains no shell metacharacters.
func canonicalCIDR(s string) (string, bool) {
	ip, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		return "", false
	}
	ones, _ := ipNet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip.String(), ones), true
}

// canonicalIP parses a bare IP and returns its canonical string form.
func canonicalIP(s string) (string, bool) {
	ip := net.ParseIP(s)
	if ip == nil {
		return "", false
	}
	return ip.String(), true
}

// netconfigIface is one entry passed to the netconfig binary via the NETCONFIG
// env var (JSON). No user value is ever interpolated into a shell.
type netconfigIface struct {
	Name    string           `json:"name"`
	Mode    string           `json:"mode"` // "static" | "dhcp-preset"
	IP      string           `json:"ip,omitempty"`
	Gateway string           `json:"gateway,omitempty"`
	Routes  []netconfigRoute `json:"routes,omitempty"`
}

type netconfigRoute struct {
	Dst string `json:"dst"`
	Via string `json:"via"`
}

// netConfigInitContainer builds the init-container that addresses interfaces the
// device image does not handle itself: static (apply the fixed IP/routes) and
// dhcp-preset (lease a dynamic address for images with no DHCP client). Values
// are canonicalised and passed as JSON, so nothing user-controlled reaches a
// shell. Returns nil when there is no such interface or no NetConfigImage.
func (r *DeviceReconciler) netConfigInitContainer(device *laboratoryv1alpha1.Device) *corev1.Container {
	if r.NetConfigImage == "" {
		return nil
	}
	var ifaces []netconfigIface
	for _, iface := range device.Spec.Interfaces {
		name, ok := validIfaceName(iface.Name)
		if !ok {
			continue
		}
		switch iface.Addr.Type {
		case laboratoryv1alpha1.AddrTypeStatic:
			ipCIDR, ok := canonicalCIDR(iface.Addr.IP)
			if !ok {
				continue
			}
			nc := netconfigIface{Name: name, Mode: "static", IP: ipCIDR}
			if gw, ok := canonicalIP(iface.Addr.Gateway); ok {
				nc.Gateway = gw
			}
			for _, rt := range iface.Addr.Routes {
				if dst, dok := canonicalCIDR(rt.Dst); dok {
					if via, vok := canonicalIP(rt.Via); vok {
						nc.Routes = append(nc.Routes, netconfigRoute{Dst: dst, Via: via})
					}
				}
			}
			ifaces = append(ifaces, nc)
		case laboratoryv1alpha1.AddrTypeDHCPPreset:
			ifaces = append(ifaces, netconfigIface{Name: name, Mode: "dhcp-preset"})
		}
	}
	if len(ifaces) == 0 {
		return nil
	}
	cfg, err := json.Marshal(ifaces)
	if err != nil {
		return nil
	}
	return &corev1.Container{
		Name:            "netconfig",
		Image:           r.NetConfigImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/netconfig"},
		Env:             []corev1.EnvVar{{Name: "NETCONFIG", Value: string(cfg)}},
		SecurityContext: &corev1.SecurityContext{
			// NET_ADMIN to set addresses/routes; NET_RAW for the DHCP raw socket.
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"}},
		},
	}
}

// deviceSecurityContext resolves the device's SecurityPreset to concrete
// capabilities and adds the DHCP-implied caps when the image runs its own DHCP
// client (addr.type=dhcp). Returns nil when nothing is needed, so a basic
// service device stays fully unprivileged.
func deviceSecurityContext(device *laboratoryv1alpha1.Device) *corev1.SecurityContext {
	want := map[string]bool{}
	for _, c := range names.CapabilitiesForPreset(string(device.Spec.SecurityPreset)) {
		want[c] = true
	}
	if deviceHasInImageDHCP(device) {
		for _, c := range names.DHCPImpliedCapabilities {
			want[c] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	caps := make([]corev1.Capability, 0, len(want))
	for c := range want {
		caps = append(caps, corev1.Capability(c))
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{Add: caps},
	}
}

// deviceHasInImageDHCP reports whether any interface expects the image's own
// DHCP client (addr.type=dhcp) — those need pod capabilities. "managed" DHCP is
// handled by the netconfig init-container and needs no device-container caps.
func deviceHasInImageDHCP(device *laboratoryv1alpha1.Device) bool {
	for _, iface := range device.Spec.Interfaces {
		if iface.Addr.Type == laboratoryv1alpha1.AddrTypeDHCP {
			return true
		}
	}
	return false
}

func (r *DeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Device{}).
		Owns(&appsv1.Deployment{}).
		// A switch/hub's readiness depends on its Connections — re-reconcile the
		// referenced devices whenever a Connection changes.
		Watches(&laboratoryv1alpha1.Connection{}, handler.EnqueueRequestsFromMapFunc(r.devicesForConnection)).
		Complete(r)
}

// devicesForConnection maps a Connection to reconcile requests for the devices
// at its endpoints (CR name is "<labRef>-<device>"). Enqueuing a non-switch
// device is harmless — its reconcile ignores Connections.
func (r *DeviceReconciler) devicesForConnection(_ context.Context, obj client.Object) []reconcile.Request {
	conn, ok := obj.(*laboratoryv1alpha1.Connection)
	if !ok {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(conn.Spec.Endpoints))
	for _, e := range conn.Spec.Endpoints {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      fmt.Sprintf("%s-%s", conn.Spec.LabRef, e.Device),
			Namespace: conn.Namespace,
		}})
	}
	return reqs
}
