package laboratory

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
)

// The gRPC access of a device (EDA web-direct, programmatic level) is published like the web port: a headless Service named by the
// access host of the kind, <device>-<code>g, selecting the device pod, and a NetworkPolicy that lets only the L7 proxy in, on that
// port. The Service carries LabelAccessKind so that it is never mistaken for the web Service of the device. Both are made once the
// Device exists, because the host needs its code; a device without a code (a VM, or one that predates codes) has no gRPC host.

// grpcServiceName is the host label and the Service name of the gRPC access of a device, or false while the device has no code.
func grpcServiceName(device, code string) (string, bool) {
	return names.AccessHostLabel(device, code, names.AccessKindGRPC)
}

// proxyIngressPeers are the sources the L7 proxy reaches a device from: its pods, and the node CIDRs when it runs on the host network.
func (r *LabReconciler) proxyIngressPeers() []networkingv1.NetworkPolicyPeer {
	peers := []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": names.ProxyNamespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: names.ProxyL7App}},
	}}
	for _, cidr := range r.ProxySourceCIDRs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	return peers
}

// ensureGRPCServices creates the Service and the NetworkPolicy of every gRPC access of the lab, and removes those of a device that no
// longer has one.
func (r *LabReconciler) ensureGRPCServices(ctx context.Context, lab *laboratoryv1alpha1.Lab, snapshots ...*labServiceSnapshot) error {
	want := map[string]bool{}
	services := r.serviceSnapshot(lab, snapshots...)
	for _, d := range lab.Spec.Devices {
		access := laboratoryv1alpha1.GRPCAccess(d.Access)
		if access == nil {
			continue
		}
		dev, err := devices.Get(ctx, r.reader(), lab.Namespace, lab.Name, d.Name)
		if err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue // the Device is not there yet; its creation queues the lab again
			}
			return fmt.Errorf("find device %s: %w", d.Name, err)
		}
		svcName, ok := grpcServiceName(d.Name, dev.Spec.Code)
		if !ok {
			continue
		}
		want[svcName] = true
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: lab.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			if svc.Labels == nil {
				svc.Labels = map[string]string{}
			}
			svc.Labels[names.LabelLab] = lab.Name
			svc.Labels[names.LabelDevice] = d.Name
			svc.Labels[names.LabelAccessKind] = names.AccessKindGRPC
			svc.Spec.Selector = map[string]string{names.LabelLab: lab.Name, names.LabelDevice: d.Name}
			svc.Spec.Ports = []corev1.ServicePort{{
				Name: names.AccessKindGRPC, Port: access.Port, TargetPort: intstr.FromInt32(access.Port), Protocol: corev1.ProtocolTCP,
			}}
			svc.Spec.Type = corev1.ServiceTypeClusterIP
			svc.Spec.ClusterIP = corev1.ClusterIPNone
			return controllerutil.SetOwnerReference(lab, svc, r.Scheme)
		}); err != nil {
			return fmt.Errorf("ensure gRPC Service of device %s: %w", d.Name, err)
		}

		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: svcName + "-grpc", Namespace: lab.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			tcp := corev1.ProtocolTCP
			np.Spec = networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{names.LabelLab: lab.Name, names.LabelDevice: d.Name}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  r.proxyIngressPeers(),
					Ports: []networkingv1.NetworkPolicyPort{{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: access.Port}, Protocol: &tcp}},
				}},
				Egress: []networkingv1.NetworkPolicyEgressRule{},
			}
			return controllerutil.SetOwnerReference(lab, np, r.Scheme)
		}); err != nil {
			return fmt.Errorf("ensure gRPC NetworkPolicy of device %s: %w", d.Name, err)
		}
	}
	return r.pruneGRPCServices(ctx, lab, services, want)
}

// pruneGRPCServices removes the gRPC Services (and their policies) of the lab that the spec no longer wants.
func (r *LabReconciler) pruneGRPCServices(ctx context.Context, lab *laboratoryv1alpha1.Lab, services *labServiceSnapshot, want map[string]bool) error {
	existing, err := services.grpcServices(ctx)
	if err != nil {
		return err
	}
	for _, svc := range existing {
		if want[svc.Name] {
			continue
		}
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: svc.Name + "-grpc", Namespace: lab.Namespace}}
		if err := r.Delete(ctx, np); client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// grpcAccessEntries is one entry per device with a gRPC access: the port, protocol "grpc", and the host of the access once the device has its code.
func (r *LabReconciler) grpcAccessEntries(ctx context.Context, lab *laboratoryv1alpha1.Lab) []laboratoryv1alpha1.AccessEntry {
	var out []laboratoryv1alpha1.AccessEntry
	for _, d := range lab.Spec.Devices {
		access := laboratoryv1alpha1.GRPCAccess(d.Access)
		if access == nil {
			continue
		}
		entry := laboratoryv1alpha1.AccessEntry{Device: d.Name, Port: access.Port, Protocol: string(laboratoryv1alpha1.AccessGRPC)}
		if r.BaseDomain != "" {
			if dev, err := devices.Get(ctx, r.reader(), lab.Namespace, lab.Name, d.Name); err == nil {
				if label, ok := grpcServiceName(d.Name, dev.Spec.Code); ok {
					entry.URL = fmt.Sprintf("https://%s.%s", label, r.BaseDomain)
				}
			}
		}
		out = append(out, entry)
	}
	return out
}
