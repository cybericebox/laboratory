package laboratory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

const (
	finalizerLab     = "cybericebox.com/lab"
	finalizerVPN     = "cybericebox.com/vpn"
	finalizerGateway = "cybericebox.com/gateway"

	vniPoolNS     = "lab-system"
	vniPoolPrefix = "vni"
	vniPoolSize   = uint(65000)

	labSubnetPool    = "lab-subnets"
	vpnSubnetOctet2  = 8 // 10.8.N.0/24
	inetSubnetOctet2 = 9 // 10.9.N.0/24
)

// LabReconciler reconciles a Lab object.
type LabReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labs/finalizers,verbs=update
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices;connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=devices/status;connections/status,verbs=get;update;patch

func (r *LabReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var lab laboratoryv1alpha1.Lab
	if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !lab.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lab)
	}

	if !controllerutil.ContainsFinalizer(&lab, finalizerLab) {
		controllerutil.AddFinalizer(&lab, finalizerLab)
		if err := r.Update(ctx, &lab); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureNetworkFinalizers(ctx, &lab); err != nil {
		return ctrl.Result{}, err
	}
	if updated, err := r.ensureSubnetAllocation(ctx, &lab); err != nil {
		return ctrl.Result{}, err
	} else if updated {
		// Re-fetch after status update so we have the latest resourceVersion.
		if err := r.Get(ctx, req.NamespacedName, &lab); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if err := r.ensureWebServices(ctx, &lab); err != nil {
		logger.Error(err, "ensure web services")
		return ctrl.Result{}, err
	}

	if err := r.validateGraph(&lab); err != nil {
		lab.Status.Phase = laboratoryv1alpha1.PhaseFailed
		if statusErr := r.Status().Update(ctx, &lab); statusErr != nil {
			logger.Error(statusErr, "update status after graph validation failure")
		}
		return ctrl.Result{}, nil
	}

	if err := r.materializeDevices(ctx, &lab); err != nil {
		logger.Error(err, "materialize devices")
		return ctrl.Result{}, err
	}

	if err := r.materializeConnections(ctx, &lab); err != nil {
		logger.Error(err, "materialize connections")
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, &lab)
}

// validateGraph checks for switch/hub cycles in the connection graph.
func (r *LabReconciler) validateGraph(lab *laboratoryv1alpha1.Lab) error {
	switchDevices := map[string]bool{}
	for _, d := range lab.Spec.Devices {
		if d.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch || d.Type == laboratoryv1alpha1.DeviceTypeHub {
			switchDevices[d.Name] = true
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
	return nil
}

func (r *LabReconciler) materializeDevices(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	vniAllocator := poolpkg.NewAllocator(r.Client, vniPoolPrefix, vniPoolNS, vniPoolSize)

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
				Labels:     map[string]string{laboratoryv1alpha1.LabelLab: lab.Name},
				Finalizers: []string{laboratoryv1alpha1.FinalizerOVSCleanup},
			},
			Spec: laboratoryv1alpha1.DeviceSpec{
				LabRef:     lab.Name,
				Name:       tmpl.Name,
				Type:       tmpl.Type,
				Image:      tmpl.Image,
				Interfaces: tmpl.Interfaces,
				Exposure:   tmpl.Exposure,
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

func isSwitchDevice(name string, lab *laboratoryv1alpha1.Lab) bool {
	for _, d := range lab.Spec.Devices {
		if d.Name == name {
			return d.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch || d.Type == laboratoryv1alpha1.DeviceTypeHub
		}
	}
	return false
}

func (r *LabReconciler) materializeConnections(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	vniAllocator := poolpkg.NewAllocator(r.Client, vniPoolPrefix, vniPoolNS, vniPoolSize)

	for _, tmpl := range lab.Spec.Connections {
		connName := connectionName(lab.Name, tmpl.Endpoints)
		var existing laboratoryv1alpha1.Connection
		if err := r.Get(ctx, types.NamespacedName{Name: connName, Namespace: lab.Namespace}, &existing); err == nil {
			continue
		} else if !errors.IsNotFound(err) {
			return err
		}

		isDirect := true
		for _, ep := range tmpl.Endpoints {
			if isSwitchDevice(ep.Device, lab) {
				isDirect = false
				break
			}
		}

		conn := &laboratoryv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{
				Name:       connName,
				Namespace:  lab.Namespace,
				Labels:     map[string]string{laboratoryv1alpha1.LabelLab: lab.Name},
				Finalizers: []string{laboratoryv1alpha1.FinalizerOVSCleanup},
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

// connectionName produces a deterministic, length-bounded Connection name from lab name + endpoints.
// Endpoints are sorted so ordering differences in the spec don't produce different names.
func connectionName(labName string, endpoints []laboratoryv1alpha1.EndpointSpec) string {
	parts := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		if ep.Interface != "" {
			parts = append(parts, ep.Device+"-"+ep.Interface)
		} else {
			parts = append(parts, ep.Device)
		}
	}
	sort.Strings(parts)
	full := labName + "--" + strings.Join(parts, "--")
	if len(full) <= 253 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(sum[:4])
	prefix := strings.TrimRight(full[:243], "-")
	return prefix + "--" + suffix
}

func (r *LabReconciler) updateStatus(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	var deviceList laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &deviceList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{laboratoryv1alpha1.LabelLab: lab.Name}); err != nil {
		return ctrl.Result{}, err
	}

	var refs []laboratoryv1alpha1.DeviceRef
	allReady := len(deviceList.Items) > 0
	for _, d := range deviceList.Items {
		refs = append(refs, laboratoryv1alpha1.DeviceRef{Name: d.Spec.Name, Ready: d.Status.Ready})
		if !d.Status.Ready {
			allReady = false
		}
	}

	var connList laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &connList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{laboratoryv1alpha1.LabelLab: lab.Name}); err != nil {
		return ctrl.Result{}, err
	}

	var connRefs []laboratoryv1alpha1.ConnectionRef
	for _, c := range connList.Items {
		connRefs = append(connRefs, laboratoryv1alpha1.ConnectionRef{Name: c.Name, Ready: c.Status.Ready})
		if !c.Status.Ready {
			allReady = false
		}
	}

	newPhase := laboratoryv1alpha1.PhaseProvisioning
	if allReady {
		newPhase = laboratoryv1alpha1.PhaseReady
	}

	if newPhase == lab.Status.Phase &&
		reflect.DeepEqual(refs, lab.Status.Devices) &&
		reflect.DeepEqual(connRefs, lab.Status.Connections) {
		return ctrl.Result{}, nil
	}

	lab.Status.Devices = refs
	lab.Status.Connections = connRefs
	lab.Status.Phase = newPhase

	return ctrl.Result{}, r.Status().Update(ctx, lab)
}

func (r *LabReconciler) reconcileDelete(ctx context.Context, lab *laboratoryv1alpha1.Lab) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var deviceList laboratoryv1alpha1.DeviceList
	if err := r.List(ctx, &deviceList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{laboratoryv1alpha1.LabelLab: lab.Name}); err != nil {
		return ctrl.Result{}, err
	}

	// Release VNIs for switch/hub devices before they are garbage-collected.
	vniAllocator := poolpkg.NewAllocator(r.Client, vniPoolPrefix, vniPoolNS, vniPoolSize)
	for i := range deviceList.Items {
		d := &deviceList.Items[i]
		if d.Status.VNI == nil {
			continue
		}
		if err := vniAllocator.ReleaseIndex(ctx, *d.Status.VNI); err != nil {
			return ctrl.Result{}, fmt.Errorf("release switch VNI %d for device %s: %w", *d.Status.VNI, d.Name, err)
		}
		d.Status.VNI = nil
		if err := r.Status().Update(ctx, d); err != nil {
			return ctrl.Result{}, err
		}
	}

	if len(deviceList.Items) > 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	var connList laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &connList, client.InNamespace(lab.Namespace),
		client.MatchingLabels{laboratoryv1alpha1.LabelLab: lab.Name}); err != nil {
		return ctrl.Result{}, err
	}
	if len(connList.Items) > 0 {
		for i := range connList.Items {
			c := &connList.Items[i]
			if c.Status.VNI == nil {
				continue
			}
			if err := vniAllocator.ReleaseIndex(ctx, *c.Status.VNI); err != nil {
				logger.Error(err, "release VNI", "connection", c.Name, "vni", *c.Status.VNI)
			}
			c.Status.VNI = nil
			if err := r.Status().Update(ctx, c); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Release lab subnet index if allocated.
	if lab.Status.VPN.CIDR != "" || lab.Status.Internet.CIDR != "" {
		cidr := lab.Status.VPN.CIDR
		if cidr == "" {
			cidr = lab.Status.Internet.CIDR
		}
		parts := strings.Split(cidr, ".")
		if len(parts) >= 3 {
			var n uint
			if _, scanErr := fmt.Sscanf(parts[2], "%d", &n); scanErr == nil {
				subnetAllocator := poolpkg.NewAllocator(r.Client, labSubnetPool, lab.Namespace, 254)
				if releaseErr := subnetAllocator.ReleaseIndex(ctx, n); releaseErr != nil {
					logger.Error(releaseErr, "release lab subnet", "n", n)
				}
			}
		}
	}

	controllerutil.RemoveFinalizer(lab, finalizerLab)
	return ctrl.Result{}, r.Update(ctx, lab)
}

// ensureSubnetAllocation allocates a /24 index N from the lab-subnets pool,
// writes Status.VPN.CIDR and/or Status.Internet.CIDR, and updates status.
// VPN and Internet share the same N for a given Lab.
func (r *LabReconciler) ensureSubnetAllocation(ctx context.Context, lab *laboratoryv1alpha1.Lab) (updated bool, err error) {
	needsVPN := lab.Spec.VPN.Enabled && lab.Status.VPN.CIDR == ""
	needsInet := lab.Spec.Internet.Enabled && lab.Status.Internet.CIDR == ""
	if !needsVPN && !needsInet {
		return false, nil
	}

	subnetAllocator := poolpkg.NewAllocator(r.Client, labSubnetPool, lab.Namespace, 254)
	n, err := subnetAllocator.AllocateIndex(ctx)
	if err != nil {
		return false, fmt.Errorf("allocate lab subnet: %w", err)
	}
	if needsVPN {
		lab.Status.VPN.CIDR = fmt.Sprintf("10.%d.%d.0/24", vpnSubnetOctet2, n)
	}
	if needsInet {
		lab.Status.Internet.CIDR = fmt.Sprintf("10.%d.%d.0/24", inetSubnetOctet2, n)
	}
	return true, r.Status().Update(ctx, lab)
}

func (r *LabReconciler) ensureNetworkFinalizers(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	changed := false
	if lab.Spec.VPN.Enabled && !controllerutil.ContainsFinalizer(lab, finalizerVPN) {
		controllerutil.AddFinalizer(lab, finalizerVPN)
		changed = true
	}
	if lab.Spec.Internet.Enabled && !controllerutil.ContainsFinalizer(lab, finalizerGateway) {
		controllerutil.AddFinalizer(lab, finalizerGateway)
		changed = true
	}
	if changed {
		return r.Update(ctx, lab)
	}
	return nil
}

func (r *LabReconciler) ensureWebServices(ctx context.Context, lab *laboratoryv1alpha1.Lab) error {
	for _, d := range lab.Spec.Devices {
		if d.Exposure == nil || d.Exposure.Web == nil {
			continue
		}
		web := d.Exposure.Web
		svcName := d.Name

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: lab.Namespace},
		}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Spec.Selector = map[string]string{
				laboratoryv1alpha1.LabelLab: lab.Name,
				"app":                       d.Name,
			}
			protocol := web.Protocol
			if protocol == "" {
				protocol = "http"
			}
			svc.Spec.Ports = []corev1.ServicePort{{
				Name:       protocol,
				Port:       web.Port,
				TargetPort: intstr.FromInt32(web.Port),
				Protocol:   corev1.ProtocolTCP,
			}}
			svc.Spec.Type = corev1.ServiceTypeClusterIP
			return controllerutil.SetOwnerReference(lab, svc, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("ensure Service %s: %w", svcName, err)
		}

		np := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: svcName + "-web", Namespace: lab.Namespace},
		}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Spec = networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": d.Name}},
				PolicyTypes: []networkingv1.PolicyType{
					networkingv1.PolicyTypeIngress,
					networkingv1.PolicyTypeEgress,
				},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "proxy-system"},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "proxy"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: web.Port},
						Protocol: func() *corev1.Protocol { p := corev1.ProtocolTCP; return &p }(),
					}},
				}},
				Egress: []networkingv1.NetworkPolicyEgressRule{},
			}
			return controllerutil.SetOwnerReference(lab, np, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("ensure NetworkPolicy %s: %w", svcName, err)
		}
	}
	return nil
}

func (r *LabReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Lab{}).
		Owns(&laboratoryv1alpha1.Device{}).
		Owns(&laboratoryv1alpha1.Connection{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
