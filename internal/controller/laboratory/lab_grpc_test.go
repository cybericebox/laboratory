package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
)

func grpcLab() *laboratoryv1alpha1.Lab {
	lab := webLab("lab1", "uid-1")
	lab.Spec.Devices[0].Name = "bot"
	lab.Spec.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessGRPC, Port: 9099}}
	return lab
}

func grpcDevice(code string) *laboratoryv1alpha1.Device {
	return &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: devices.Name("lab1", "bot"), Namespace: "team-alpha"},
		Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: "lab1", Name: "bot", Code: code},
	}
}

// The gRPC access is a Service <device>-<code>g plus a policy that admits only the proxy on its port; the web Service is a different one.
func TestEnsureGRPCServicePublishesTheDeviceToTheProxyOnly(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(grpcDevice("k7x")).Build()
	r := &LabReconciler{Client: c, Scheme: s, BaseDomain: "labs.example.com"}
	lab := grpcLab()
	ctx := context.Background()
	for i := 0; i < 2; i++ { // idempotent
		if err := r.ensureGRPCServices(ctx, lab); err != nil {
			t.Fatal(err)
		}
	}
	svcs := webServices(t, c)
	if len(svcs) != 1 || svcs[0].Name != "bot-k7xg" || svcs[0].Labels[names.LabelAccessKind] != "grpc" ||
		svcs[0].Spec.Ports[0].Port != 9099 || svcs[0].Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("services: %+v", svcs)
	}
	var nps networkingv1.NetworkPolicyList
	if err := c.List(ctx, &nps, client.InNamespace("team-alpha")); err != nil || len(nps.Items) != 1 {
		t.Fatalf("policies: %v %+v", err, nps.Items)
	}
	spec := nps.Items[0].Spec
	if len(spec.Egress) != 0 || len(spec.PolicyTypes) != 2 || len(spec.Ingress) != 1 || spec.Ingress[0].Ports[0].Port.IntVal != 9099 {
		t.Fatalf("policy: %+v", spec)
	}
	for _, peer := range spec.Ingress[0].From {
		if peer.IPBlock == nil && (peer.PodSelector == nil || peer.PodSelector.MatchLabels["app"] != names.ProxyL7App) {
			t.Errorf("a peer that is not the proxy: %+v", peer)
		}
	}

	entries := r.grpcAccessEntries(ctx, lab)
	if len(entries) != 1 || entries[0].Protocol != "grpc" || entries[0].Port != 9099 || entries[0].URL != "https://bot-k7xg.labs.example.com" {
		t.Fatalf("entries: %+v", entries)
	}

	// the access leaves the spec: the Service and its policy go
	lab.Spec.Devices[0].Access = nil
	if err := r.ensureGRPCServices(ctx, lab); err != nil {
		t.Fatal(err)
	}
	if got := webServices(t, c); len(got) != 0 {
		t.Fatalf("services left: %+v", got)
	}
	if err := c.List(ctx, &nps, client.InNamespace("team-alpha")); err != nil || len(nps.Items) != 0 {
		t.Fatalf("policies left: %v %+v", err, nps.Items)
	}
}

// Without a Device (or its code) there is no host: nothing is created and no link is given.
func TestEnsureGRPCServiceWaitsForTheDeviceCode(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	for _, objs := range [][]client.Object{nil, {grpcDevice("")}} {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		r := &LabReconciler{Client: c, Scheme: s, BaseDomain: "labs.example.com"}
		if err := r.ensureGRPCServices(context.Background(), grpcLab()); err != nil {
			t.Fatal(err)
		}
		if got := webServices(t, c); len(got) != 0 {
			t.Fatalf("services: %+v", got)
		}
		if e := r.grpcAccessEntries(context.Background(), grpcLab()); len(e) != 1 || e[0].URL != "" {
			t.Fatalf("entries: %+v", e)
		}
	}
}

// The gRPC Service is not the web Service of the device: the snapshot used for the web host and the code never returns it.
func TestServiceSnapshotIgnoresTheGRPCService(t *testing.T) {
	lab := grpcLab()
	snap := &labServiceSnapshot{lab: lab}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "bot-k7xg", Namespace: "team-alpha",
		Labels:          map[string]string{names.LabelLab: "lab1", names.LabelDevice: "bot", names.LabelAccessKind: "grpc"},
		OwnerReferences: []metav1.OwnerReference{{UID: lab.UID, Kind: ownerKindLab}},
	}}
	snap.remember(svc)
	if snap.byDevice["bot"] != nil {
		t.Fatal("the gRPC Service was taken for the web Service")
	}
}

// The runner behind the gRPC port has no transport authentication of its own in h2c mode: the policy must admit the proxy pods, on that one
// port, and nothing else. The object is compared whole, so a broader selector, another port or a second rule fails here.
func TestGRPCNetworkPolicyIsExactlyTheProxyOnOnePort(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	for _, cidrs := range [][]string{nil, {"10.1.2.3/32", "10.1.2.4/32"}} {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(grpcDevice("k7x")).Build()
		r := &LabReconciler{Client: c, Scheme: s, BaseDomain: "labs.example.com", ProxySourceCIDRs: cidrs}
		if err := r.ensureGRPCServices(context.Background(), grpcLab()); err != nil {
			t.Fatal(err)
		}
		var np networkingv1.NetworkPolicy
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-alpha", Name: "bot-k7xg-grpc"}, &np); err != nil {
			t.Fatal(err)
		}
		tcp := corev1.ProtocolTCP
		from := []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": names.ProxyNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": names.ProxyL7App}},
		}}
		for _, cidr := range cidrs {
			from = append(from, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
		}
		want := networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "bot"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  from,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 9099}}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{},
		}
		if !equality.Semantic.DeepEqual(np.Spec, want) {
			t.Fatalf("cidrs %v:\n got  %+v\n want %+v", cidrs, np.Spec, want)
		}
	}
}
