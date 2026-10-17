package demux

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func addrs(t *testing.T, in ...string) []netip.AddrPort {
	t.Helper()
	out := make([]netip.AddrPort, 0, len(in))
	for _, s := range in {
		out = append(out, netip.MustParseAddrPort(s))
	}
	return out
}

func testNode(addresses ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: corev1.NodeStatus{Addresses: addresses}}
}

func udpPort(port, nodePort int32) corev1.ServicePort {
	return corev1.ServicePort{Protocol: corev1.ProtocolUDP, Port: port, NodePort: nodePort}
}

func TestEntryAddrsCloud(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{udpPort(51820, 31820)}}}
	node := testNode(
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.10"},
		corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "3.3.3.3"},
		corev1.NodeAddress{Type: corev1.NodeHostName, Address: "node-1"},
	)
	got := EntryAddrs(svc, node, "10.0.1.10")
	if want := addrs(t, "3.3.3.3:31820", "10.0.1.10:31820"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEntryAddrsOnPrem(t *testing.T) {
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{udpPort(51820, 31000)}, ExternalIPs: []string{"198.51.100.7"}},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}, {Hostname: "lb.example"}},
		}},
	}
	node := testNode(corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.10"})
	got := EntryAddrs(svc, node, "10.0.1.10")
	want := addrs(t, "10.0.1.10:31000", "192.0.2.10:51820", "198.51.100.7:51820")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEntryAddrsHostIPAndNoNodePort(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{udpPort(51820, 0)}}}
	if got := EntryAddrs(svc, testNode(corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.10"}), "10.0.1.10"); len(got) != 0 {
		t.Fatalf("no nodePort and no LB must give nothing, got %v", got)
	}
	svc.Spec.Ports[0].NodePort = 31820
	got := EntryAddrs(svc, nil, "10.0.1.11")
	if want := addrs(t, "10.0.1.11:31820"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEntryAddrsSkipsIPv6AndTCP(t *testing.T) {
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Ports:       []corev1.ServicePort{udpPort(51820, 31820), {Protocol: corev1.ProtocolTCP, Port: 8080, NodePort: 30080}},
			ExternalIPs: []string{"2001:db8::1", "not-an-ip"},
		},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "2001:db8::2"}}}},
	}
	node := testNode(
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::1"},
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.10"},
	)
	got := EntryAddrs(svc, node, "fd00::1")
	if want := addrs(t, "10.0.1.10:31820"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEntryAddrsDeduplicatesAndSorts(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{udpPort(51820, 31820)}}}
	node := testNode(
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.10"},
		corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "10.0.0.9"},
	)
	got := EntryAddrs(svc, node, "10.0.1.10")
	if want := addrs(t, "10.0.0.9:31820", "10.0.1.10:31820"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEntryWatcherCallsOnChange(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "laboratory-proxy-wg", Namespace: "laboratory-wg"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{udpPort(51820, 31820)}},
	}
	node := testNode(corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.1.10"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, node).Build()

	var calls [][]netip.AddrPort
	w := &EntryWatcher{
		Client: c, NodeName: "node-1", HostIP: "10.0.1.10", ServiceName: "laboratory-proxy-wg", ServiceNamespace: "laboratory-wg",
		OnChange: func(a []netip.AddrPort) { calls = append(calls, a) },
	}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "laboratory-wg", Name: "laboratory-proxy-wg"}}
	reconcile := func() {
		t.Helper()
		if _, err := w.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}

	reconcile()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], addrs(t, "10.0.1.10:31820")) {
		t.Fatalf("first reconcile: %v", calls)
	}
	reconcile()
	if len(calls) != 1 {
		t.Fatalf("unchanged set must not call OnChange, got %v", calls)
	}

	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := c.Status().Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], addrs(t, "10.0.1.10:31820", "192.0.2.10:51820")) {
		t.Fatalf("after status update: %v", calls)
	}

	if err := c.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if len(calls) != 3 || len(calls[2]) != 0 {
		t.Fatalf("after service delete want an empty set, got %v", calls)
	}
}

func TestCacheOptionsRestrictsToOneServiceAndNode(t *testing.T) {
	opts := CacheOptions("laboratory-wg", "laboratory-proxy-wg", "node-1")
	byObject := map[string]bool{}
	for obj, o := range opts.ByObject {
		switch obj.(type) {
		case *corev1.Service:
			byObject["service"] = true
			if got := o.Field.String(); got != "metadata.name=laboratory-proxy-wg" {
				t.Fatalf("service field selector %q", got)
			}
			if _, ok := o.Namespaces["laboratory-wg"]; !ok || len(o.Namespaces) != 1 {
				t.Fatalf("service namespaces %v", o.Namespaces)
			}
		case *corev1.Node:
			byObject["node"] = true
			if got := o.Field.String(); got != "metadata.name=node-1" {
				t.Fatalf("node field selector %q", got)
			}
		}
	}
	if !byObject["service"] || !byObject["node"] {
		t.Fatalf("missing ByObject entries: %v", byObject)
	}
}
