package laboratory

import (
	"context"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func vpnLab(name string, cidrs map[string]string) *laboratoryv1alpha1.Lab {
	l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g"}}
	for _, g := range []string{"office", "dmz"} {
		if cidr, ok := cidrs[g]; ok {
			l.Spec.Gateways = append(l.Spec.Gateways, laboratoryv1alpha1.GatewayTemplate{Name: g, Kind: laboratoryv1alpha1.GatewayKindVPN})
			l.Status.Gateways = append(l.Status.Gateways, laboratoryv1alpha1.GatewayStatus{Name: g, Kind: laboratoryv1alpha1.GatewayKindVPN, CIDR: cidr})
		}
	}
	return l
}

// The client routes into the tunnel exactly what its rules let it reach (union over connectors), not the whole lab base network.
func TestClientAllowedIPsAreTheReachableSet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := &laboratoryv1alpha1.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "g"}, Spec: laboratoryv1alpha1.LabGroupAccessPolicySpec{Rules: []laboratoryv1alpha1.LabGroupAccessRule{
		{Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{"alice"}, LabNames: []string{"web"}, GatewayNames: []string{"dmz"}},
		{Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{"alice"}, LabNames: []string{"ctf"}, GatewayNames: []string{"office"}, Include: []laboratoryv1alpha1.AccessInclude{{Host: 7, Prefix: 32}, {Host: 128, Prefix: 25}}},
	}}}
	r := &LabGroupClientReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy,
		vpnLab("web", map[string]string{"office": "10.8.1.0/24", "dmz": "10.8.2.0/24"}), vpnLab("ctf", map[string]string{"office": "10.8.3.0/24"})).Build()}
	alice := &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "g"}}
	got, err := r.reachableDestinations(context.Background(), alice, "10.8.0.2/32")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.8.2.0/24", "10.8.3.128/25", "10.8.3.7/32"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedIPs = %v, want %v", got, want)
	}
	// A client with no rule gets no routes; a group without a policy yet likewise.
	if got, _ := r.reachableDestinations(context.Background(), &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "g"}}, "10.8.0.3/32"); len(got) != 0 {
		t.Fatalf("bob = %v", got)
	}
	empty := &LabGroupClientReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	if got, err := empty.reachableDestinations(context.Background(), alice, "10.8.0.2/32"); err != nil || len(got) != 0 {
		t.Fatalf("no policy: %v %v", got, err)
	}
}

func TestRenderedConfigListsExactlyTheAllowedIPs(t *testing.T) {
	p := wgConfParams{AssignedIP: "10.8.0.2/32", ServerPublicKey: "k", Endpoint: "vpn.example:51820", AllowedIPs: "10.8.2.0/24, 10.8.3.7/32"}
	conf, err := renderWGConf(p)
	if err != nil || !strings.Contains(conf, "AllowedIPs = 10.8.2.0/24, 10.8.3.7/32\n") {
		t.Fatalf("%q %v", conf, err)
	}
	p.AllowedIPs = ""
	conf, err = renderWGConf(p)
	if err != nil || strings.Contains(conf, "AllowedIPs") || !strings.Contains(conf, "PersistentKeepalive = 15") {
		t.Fatalf("a client that reaches nothing routes nothing: %q %v", conf, err)
	}
}
