package laboratory

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
			Containers: []corev1.Container{{
				Name:  device.Spec.Name,
				Image: device.Spec.Image,
				// Run the image as-is. Device pods get no elevated capabilities
				// and no entrypoint override: interfaces are wired and addressed
				// from outside the container. Only gateway/VPN pods (not devices)
				// run privileged.
			}},
		},
	}
	// Optional init-container: assign static IP/routes inside the pod netns.
	// node-agent only wires the L2 veth; addressing is applied here so device
	// pods need no elevated capabilities of their own. DHCP interfaces are left
	// for the in-pod client. Skipped entirely when no interface is static.
	if ic := r.staticAddrInitContainer(device); ic != nil {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, *ic)
	}

	if err := controllerutil.SetControllerReference(device, pod, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pod)
}

// staticAddrInitContainer builds an init-container that waits for each
// static-addressed interface (moved in by node-agent) and assigns its IP,
// gateway, and routes. Returns nil when the device has no static interfaces or
// no NetConfigImage is configured.
func (r *DeviceReconciler) staticAddrInitContainer(device *laboratoryv1alpha1.Device) *corev1.Container {
	if r.NetConfigImage == "" {
		return nil
	}
	var cmds []string
	for _, iface := range device.Spec.Interfaces {
		if iface.Addr.Type != laboratoryv1alpha1.AddrTypeStatic || iface.Addr.IP == "" {
			continue
		}
		name := iface.Name
		// Wait up to ~5s for node-agent to move the veth into this netns.
		cmds = append(cmds, "for i in $(seq 1 25); do ip link show "+name+" >/dev/null 2>&1 && break; sleep 0.2; done")
		cmds = append(cmds, "ip addr replace "+iface.Addr.IP+" dev "+name)
		cmds = append(cmds, "ip link set "+name+" up")
		if iface.Addr.Gateway != "" {
			cmds = append(cmds, "ip route replace default via "+iface.Addr.Gateway)
		}
		for _, rt := range iface.Addr.Routes {
			if rt.Dst != "" && rt.Via != "" {
				cmds = append(cmds, "ip route replace "+rt.Dst+" via "+rt.Via)
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return &corev1.Container{
		Name:            "netconfig",
		Image:           r.NetConfigImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", strings.Join(cmds, "\n")},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN"},
			},
		},
	}
}

func (r *DeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Device{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
