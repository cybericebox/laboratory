package demux

import (
	"context"
	"net/netip"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// EntryAddrs computes the address:port pairs the fast path matches on this node: the node addresses with each UDP nodePort, and the
// load balancer and external IPs with each UDP port. Only IPv4 is returned (the fast path is IPv4, IPv6 stays on the slow path).
// The result is sorted and has no duplicates. Either object may be nil.
func EntryAddrs(svc *corev1.Service, node *corev1.Node, hostIP string) []netip.AddrPort {
	if svc == nil {
		return nil
	}
	var nodeIPs, serviceIPs []netip.Addr
	if node != nil {
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				nodeIPs = appendIPv4(nodeIPs, a.Address)
			}
		}
	}
	nodeIPs = appendIPv4(nodeIPs, hostIP)
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		serviceIPs = appendIPv4(serviceIPs, ing.IP)
	}
	for _, ip := range svc.Spec.ExternalIPs {
		serviceIPs = appendIPv4(serviceIPs, ip)
	}

	var out []netip.AddrPort
	for _, p := range svc.Spec.Ports {
		if p.Protocol != corev1.ProtocolUDP {
			continue
		}
		if p.NodePort > 0 {
			for _, ip := range nodeIPs {
				out = append(out, netip.AddrPortFrom(ip, uint16(p.NodePort)))
			}
		}
		if p.Port > 0 {
			for _, ip := range serviceIPs {
				out = append(out, netip.AddrPortFrom(ip, uint16(p.Port)))
			}
		}
	}
	slices.SortFunc(out, netip.AddrPort.Compare)
	return slices.Compact(out)
}

func appendIPv4(list []netip.Addr, s string) []netip.Addr {
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() {
		return list
	}
	return append(list, ip)
}

// EntryWatcher keeps the entry addresses current. It reads the one Service and the one Node of this demux; the manager cache is
// restricted to exactly those two objects (see CacheOptions), so any event in it is an event of one of them. OnChange gets the full
// sorted set whenever it differs from the last one.
type EntryWatcher struct {
	client.Client
	NodeName         string
	HostIP           string
	ServiceName      string
	ServiceNamespace string
	OnChange         func([]netip.AddrPort)

	mu   sync.Mutex
	last []netip.AddrPort
	sent bool
}

func (w *EntryWatcher) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	var svc *corev1.Service
	var s corev1.Service
	switch err := w.Get(ctx, types.NamespacedName{Namespace: w.ServiceNamespace, Name: w.ServiceName}, &s); {
	case err == nil:
		svc = &s
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	var node *corev1.Node
	var n corev1.Node
	switch err := w.Get(ctx, types.NamespacedName{Name: w.NodeName}, &n); {
	case err == nil:
		node = &n
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	next := EntryAddrs(svc, node, w.HostIP)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sent && slices.Equal(w.last, next) {
		return ctrl.Result{}, nil
	}
	w.last, w.sent = next, true
	if w.OnChange != nil {
		w.OnChange(slices.Clone(next))
	}
	return ctrl.Result{}, nil
}

// SetupWithManager reconciles one fixed key on any Service or Node event: the result depends on both objects, not on which one moved.
func (w *EntryWatcher) SetupWithManager(mgr ctrl.Manager) error {
	key := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: w.ServiceNamespace, Name: w.ServiceName}}
	toKey := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{key}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("wg-demux-entry").
		Watches(&corev1.Service{}, toKey).
		Watches(&corev1.Node{}, toKey).
		Complete(reconcileutil.Quiet(w))
}

// CacheOptions restricts a manager's cache to the one Service and the one Node, so the demux needs get/list/watch only on those
// (services in its namespace, and nodes) and does not hold the cluster's objects in memory.
func CacheOptions(namespace, serviceName, nodeName string) cache.Options {
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Service{}: {
			Namespaces: map[string]cache.Config{namespace: {}},
			Field:      fields.OneTermEqualSelector("metadata.name", serviceName),
		},
		&corev1.Node{}: {Field: fields.OneTermEqualSelector("metadata.name", nodeName)},
	}}
}
