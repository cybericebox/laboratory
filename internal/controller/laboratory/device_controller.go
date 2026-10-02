package laboratory

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/netattach"
	"github.com/cybericebox/laboratory/internal/profiles"
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
	// Defaults are the CPU and memory of a device container that declares none.
	Defaults DeviceDefaults
	// Registry is the snapshot registry, nil when state persistence is off; it
	// is used to drop a device's snapshots on reset.
	Registry SnapshotRegistry
	// Reader reads from the API server without the cache; nil means Client. Used
	// where a stale cache would start a device twice.
	Reader client.Reader
	// MirrorRegistries are the upstream registries the image cache serves; a
	// device whose spec names a cache prefix (Spec.ImageMirror) pulls its images
	// through it.
	MirrorRegistries []string
	// ExitSnapshotTimeout is how long a finished pod waits for the node-agent's
	// exit snapshot; zero means 30s.
	ExitSnapshotTimeout time.Duration
	// Now is the clock; nil means time.Now. A field so tests can move time.
	Now func() time.Time
	// Security is the hardening of the device pods (capabilities, user namespaces, ephemeral storage).
	Security PodSecurity
	// Scheduled makes the device wait for the scheduler to dispatch its pod
	// (see device_sched.go). Off: the pod is created as soon as the device is.
	Scheduled bool
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete;patch
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

	if err := r.syncPodLabels(ctx, &device); err != nil {
		return ctrl.Result{}, err
	}

	if deviceStateEnabled(&device) {
		return r.reconcilePod(ctx, &device)
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
	suspended, err := r.labGroupSuspended(ctx, device.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	replicas := int32(1)
	if suspended {
		replicas = 0
	}

	var dep appsv1.Deployment
	err = r.Get(ctx, types.NamespacedName{Name: workloadName(device), Namespace: device.Namespace}, &dep)

	if errors.IsNotFound(err) {
		if !suspended {
			if ok, gateErr := r.mayCreateWorkload(ctx, device); gateErr != nil || !ok {
				return ctrl.Result{}, gateErr // the scheduler's dispatch triggers the next reconcile
			}
		}
		return ctrl.Result{}, r.createDeployment(ctx, device, replicas)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	// A workload that already runs needs no dispatch.
	if err := r.initScheduling(ctx, device, true); err != nil {
		return ctrl.Result{}, err
	}

	// Voluntary disruption is blocked by the lab group's PodDisruptionBudget; the
	// per-device budget of older versions would overlap it (an eviction fails for
	// a pod under two budgets), so it is removed.
	if err := r.deleteLegacyPodDisruptionBudget(ctx, device); err != nil {
		return ctrl.Result{}, err
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != replicas {
		dep.Spec.Replicas = ptrInt32(replicas)
		if err := r.Update(ctx, &dep); err != nil {
			return ctrl.Result{}, err
		}
	}

	nodeName, podIP, podName := r.devicePodPlacement(ctx, device)
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
	if podName != device.Status.PodName {
		device.Status.PodName = podName
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
	if !suspended && (!ready || podIP == "" || podName == "") {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// devicePodPlacement returns the NodeName/PodIP/PodName of the device's live pod,
// found by label (the Deployment pod name carries a random suffix). On a brief
// overlap during recreation it takes the last Running match (the newest pod), so
// the node-agent binds the connection to the incoming pod's port.
func (r *DeviceReconciler) devicePodPlacement(ctx context.Context, device *laboratoryv1alpha1.Device) (nodeName, podIP, podName string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(device.Namespace),
		client.MatchingLabels{
			names.LabelLab:    device.Spec.LabRef,
			names.LabelDevice: device.Spec.Name,
		},
	); err != nil {
		return "", "", ""
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodRunning {
			nodeName, podIP, podName = p.Spec.NodeName, p.Status.PodIP, p.Name
		}
	}
	return nodeName, podIP, podName
}

// deviceNetworkAnnotation builds the network.cybericebox.com/networks annotation value.
// Lists OVS attachments only; default k8s network is controlled by AnnotationDefaultNetwork.
// The value is a JSON array of netattach.Attachment. At pod creation time we don't know
// the Connection name yet, so entries carry no name.
func deviceNetworkAnnotation(device *laboratoryv1alpha1.Device) string {
	return networkAnnotation(device, false)
}

// networkAnnotation is deviceNetworkAnnotation; with stableMAC every interface
// without an explicit MAC gets one derived from the device identity, so a
// recreated pod gets the same hardware address and therefore the same DHCP lease.
// An interface whose name or MAC is not valid is left out: the API validates both, so
// this only keeps a value that bypassed it from reaching the node.
func networkAnnotation(device *laboratoryv1alpha1.Device, stableMAC bool) string {
	var list []netattach.Attachment
	for _, iface := range device.Spec.Interfaces {
		mac := iface.MAC
		if stableMAC && (mac == "" || mac == "random") {
			mac = stableDeviceMAC(device.Namespace, device.Name, iface.Name)
		}
		if netattach.ValidateInterfaceName(iface.Name) != nil || netattach.ValidateMAC(mac) != nil {
			continue
		}
		list = append(list, netattach.Attachment{Iface: iface.Name, MAC: mac})
	}
	return netattach.Encode(list)
}

// workloadTemplate is the pod of a device, shared by the Deployment and the
// bare-Pod (state persistence) modes: the labels, selector labels, pod
// annotations and the pod spec running the device image.
func (r *DeviceReconciler) workloadTemplate(device *laboratoryv1alpha1.Device, stableMAC bool) (labels, selectorLabels, annotations map[string]string, podSpec corev1.PodSpec) {
	annotations = map[string]string{
		names.AnnotationDevice:   device.Spec.Name,
		names.AnnotationNetworks: networkAnnotation(device, stableMAC),
	}
	if device.Spec.Exposure != nil {
		annotations[names.AnnotationDefaultNetwork] = names.AccessPortIface
	} else if len(device.Spec.Interfaces) > 0 {
		annotations[names.AnnotationDefaultNetwork] = ""
	}

	// The labels the caller put on the lab (see deviceLabels) reach the pod, the
	// platform's own keys below always win.
	labels = names.PropagatedLabels(device.Labels)
	labels[names.LabelLab] = device.Spec.LabRef
	labels["app"] = device.Spec.Name
	labels[names.LabelDevice] = device.Spec.Name
	// The selector must be immutable and uniquely identify this device's pod:
	// (lab, device-name) is unique within the namespace.
	selectorLabels = map[string]string{
		names.LabelLab:    device.Spec.LabRef,
		names.LabelDevice: device.Spec.Name,
	}
	// The user labels of the lab, copied onto the device, go onto its pods too.
	wanted := userLabels(device.Labels)
	for k, v := range wanted {
		labels[k] = v
	}
	if len(wanted) > 0 {
		annotations[names.AnnotationUserLabels] = joinKeys(wanted)
	}

	podSpec = corev1.PodSpec{
		NodeSelector: r.LabNodeSelector,
		Tolerations:  r.LabTolerations,
		// Best-effort co-location: prefer scheduling this device onto a node that
		// already runs another device of the same lab, so a lab's intra-fabric
		// traffic stays node-local (no Geneve hop) whenever capacity allows. Soft
		// (preferred), so a full node never blocks a lab from being placed.
		Affinity: &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{names.LabelLab: device.Spec.LabRef},
						},
						TopologyKey: names.TopologyKeyHostname,
					},
				}},
			},
		},
		Containers: []corev1.Container{
			{
				Name:  device.Spec.Name,
				Image: r.deviceImage(device, device.Spec.Image),
				// Run the image as-is (no entrypoint override). Capabilities are
				// opt-in: an image that only serves a port gets none; one that
				// runs networking/testing tools or an in-image DHCP client gets
				// the curated set it requested (plus NET_ADMIN+NET_RAW when it
				// has a DHCP interface). Isolation is enforced host-side by OVS
				// flows, so these caps cannot break a pod out of its VNI.
				SecurityContext: deviceSecurityContext(device),
				Resources:       withTUN(withEphemeralStorage(deviceResources(device, r.Defaults), r.Security.EphemeralStorage), profiles.Get(string(device.Spec.SecurityPreset)).TUN),
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
	// The participant is root in the device: it gets no service account token, the runtime's seccomp profile, and
	// optionally its own user namespace.
	hardenPod(&podSpec, false)
	// Every device may ping: unprivileged ICMP echo sockets (a safe sysctl), so ping needs no NET_RAW.
	podSpec.SecurityContext.Sysctls = []corev1.Sysctl{{Name: "net.ipv4.ping_group_range", Value: profiles.PingGroupRange}}
	if r.Security.UserNamespaces {
		hostUsers := false
		podSpec.HostUsers = &hostUsers
	}
	return labels, selectorLabels, annotations, podSpec
}

// deviceImage is the reference the node pulls for an image of the device:
// through the image cache when the device was created for it, else as is.
func (r *DeviceReconciler) deviceImage(device *laboratoryv1alpha1.Device, image string) string {
	if device.Spec.ImageMirror == "" {
		return image
	}
	rw := imagecache.Rewriter{Prefix: device.Spec.ImageMirror, Registries: r.MirrorRegistries}
	return rw.RewritePinned(image, device.Spec.ImageDigests[image])
}

func (r *DeviceReconciler) createDeployment(ctx context.Context, device *laboratoryv1alpha1.Device, replicas int32) error {
	labels, selectorLabels, annotations, podSpec := r.workloadTemplate(device, false)
	podSpec.ImagePullSecrets = r.devicePullSecrets(ctx, device)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName(device),
			Namespace: device.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrInt32(replicas),
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

// deleteLegacyPodDisruptionBudget removes the per-device PodDisruptionBudget that
// older versions created (same name as the device, owned by it). Idempotent.
func (r *DeviceReconciler) deleteLegacyPodDisruptionBudget(ctx context.Context, device *laboratoryv1alpha1.Device) error {
	var pdb policyv1.PodDisruptionBudget
	err := r.Get(ctx, types.NamespacedName{Name: device.Name, Namespace: device.Namespace}, &pdb)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(&pdb, device) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &pdb))
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
		Image:           r.deviceImage(device, r.NetConfigImage),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/node", "netconfig"},
		Env:             []corev1.EnvVar{{Name: "NETCONFIG", Value: string(cfg)}},
		SecurityContext: &corev1.SecurityContext{
			// NET_ADMIN to set addresses/routes; NET_RAW for the DHCP raw socket; nothing else.
			Capabilities:             capsOf("NET_ADMIN", "NET_RAW"),
			AllowPrivilegeEscalation: ptrBool(false),
		},
	}
}

// deviceSecurityContext resolves the device's profile (profiles.Standard or Extended, or an old alias) to concrete
// capabilities and adds the DHCP-implied caps when the image runs its own DHCP client (addr.type=dhcp), whatever the
// profile. Every capability is dropped first; only the base set, the profile's and the DHCP ones are added back.
// Privilege escalation stays allowed on purpose: lab images run sudo and setuid binaries.
func deviceSecurityContext(device *laboratoryv1alpha1.Device) *corev1.SecurityContext {
	p := profiles.Get(string(device.Spec.SecurityPreset))
	want := append([]string(nil), profiles.Base...)
	want = append(want, p.Caps...)
	if deviceHasInImageDHCP(device) {
		want = append(want, names.DHCPImpliedCapabilities...)
	}
	return &corev1.SecurityContext{Capabilities: capsOf(want...)}
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
		// Devices with state persistence run as bare Pods owned by the Device.
		Owns(&corev1.Pod{}).
		Watches(&laboratoryv1alpha1.LabGroup{}, handler.EnqueueRequestsFromMapFunc(r.devicesForLabGroup), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// A switch/hub's readiness depends on its Connections — re-reconcile the
		// referenced devices whenever a Connection changes.
		Watches(&laboratoryv1alpha1.Connection{}, handler.EnqueueRequestsFromMapFunc(r.devicesForConnection)).
		Complete(r)
}

// labGroupSuspended resolves the LabGroup that owns this namespace. A namespace
// without a LabGroup is retained for standalone controller tests and runs its
// devices normally.
func (r *DeviceReconciler) labGroupSuspended(ctx context.Context, namespace string) (bool, error) {
	var group laboratoryv1alpha1.LabGroup
	err := r.Get(ctx, types.NamespacedName{Name: namespace}, &group)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return group.Spec.Suspended, nil
}

// devicesForLabGroup enqueues every Device in the group's namespace when its
// desired state changes, allowing resume without a user edit to a Device.
func (r *DeviceReconciler) devicesForLabGroup(ctx context.Context, obj client.Object) []reconcile.Request {
	group, ok := obj.(*laboratoryv1alpha1.LabGroup)
	if !ok {
		return nil
	}
	var devices laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &devices, client.InNamespace(laboratoryv1alpha1.LabGroupNamespaceOf(group))); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(devices.Items))
	for i := range devices.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      devices.Items[i].Name,
			Namespace: devices.Items[i].Namespace,
		}})
	}
	return requests
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
