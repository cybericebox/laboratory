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
// Format per entry: "iface@[connection][|MAC]"
// At pod creation time we don't know the Connection name yet, so entries are "iface@" or "iface@|MAC".
// ConnectionReconciler/NetworkAttachReconciler fill in the connection name later.
// The "@default" suffix is reserved for the Kubernetes default network (handled by cni-gate).
func deviceNetworkAnnotation(device *laboratoryv1alpha1.Device) string {
	var entries []string
	for _, iface := range device.Spec.Interfaces {
		entry := iface.Name + "@"
		if iface.MAC != "" {
			entry += "|" + iface.MAC
		}
		entries = append(entries, entry)
	}
	if device.Spec.Exposure != nil {
		entries = append(entries, "accessport@default")
	}
	return strings.Join(entries, ",")
}

func (r *DeviceReconciler) createPod(ctx context.Context, device *laboratoryv1alpha1.Device) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      device.Name,
			Namespace: device.Namespace,
			Labels: map[string]string{
				names.LabelLab:    device.Spec.LabRef,
				"app":             device.Spec.Name,
				names.LabelDevice: device.Spec.Name,
			},
			Annotations: map[string]string{
				names.AnnotationDevice:   device.Spec.Name,
				names.AnnotationNetworks: deviceNetworkAnnotation(device),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: r.LabNodeSelector,
			Tolerations:  r.LabTolerations,
			Containers: []corev1.Container{{
				Name:    device.Spec.Name,
				Image:   device.Spec.Image,
				Command: []string{"sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
					},
				},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(device, pod, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pod)
}

func (r *DeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Device{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
