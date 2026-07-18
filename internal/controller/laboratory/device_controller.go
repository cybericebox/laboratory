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
	
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete

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
		if !device.Status.Ready {
			device.Status.Ready = true
			return ctrl.Result{}, r.Status().Update(ctx, &device)
		}
		return ctrl.Result{}, nil
	}
	
	return r.reconcilePod(ctx, &device)
}

func (r *DeviceReconciler) reconcilePod(ctx context.Context, device *laboratoryv1alpha1.Device) (ctrl.Result, error) {
	podName := device.Name
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: device.Namespace}, &pod)
	
	if errors.IsNotFound(err) {
		return ctrl.Result{}, r.createPod(ctx, device)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	
	updated := false
	if pod.Spec.NodeName != device.Status.NodeName {
		device.Status.NodeName = pod.Spec.NodeName
		updated = true
	}
	if pod.Status.PodIP != device.Status.PodIP {
		device.Status.PodIP = pod.Status.PodIP
		updated = true
	}
	ready := pod.Status.Phase == corev1.PodRunning
	if ready != device.Status.Ready {
		device.Status.Ready = ready
		updated = true
	}
	if updated {
		if !ready {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.Status().Update(ctx, device)
		}
		return ctrl.Result{}, r.Status().Update(ctx, device)
	}
	if !ready {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
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

func (r *DeviceReconciler) createPod(ctx context.Context, device *laboratoryv1alpha1.Device) error {
	annotations := map[string]string{
		names.AnnotationDevice:   device.Spec.Name,
		names.AnnotationNetworks: deviceNetworkAnnotation(device),
	}
	if device.Spec.Exposure != nil {
		annotations[names.AnnotationDefaultNetwork] = names.AccessPortIface
	} else if len(device.Spec.Interfaces) > 0 {
		annotations[names.AnnotationDefaultNetwork] = ""
	}
	
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      device.Name,
			Namespace: device.Namespace,
			Labels: map[string]string{
				names.LabelLab:    device.Spec.LabRef,
				"app":             device.Spec.Name,
				names.LabelDevice: device.Spec.Name,
			},
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
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
				},
			},
		},
	}
	// Optional init-container: address static and dhcp-preset interfaces inside
	// the pod netns (node-agent only wires the L2 veth), so those device pods
	// need no capabilities of their own. addr.type=dhcp interfaces are handled by
	// the image's own client instead. Skipped when neither mode is present.
	if ic := r.netConfigInitContainer(device); ic != nil {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, *ic)
	}
	
	if err := controllerutil.SetControllerReference(device, pod, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pod)
}

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
		Owns(&corev1.Pod{}).
		Complete(r)
}
