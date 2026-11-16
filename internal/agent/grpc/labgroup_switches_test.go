package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// switchGroup makes a group with the given switches and plays the operator (namespace, status.namespace).
func switchGroup(t *testing.T, h *Handler, k8s kubernetes.Interface, name string, item *protobuf.LabGroupItem) {
	t.Helper()
	ctx := context.Background()
	item.Name = name
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{item}})
	wantStates(t, res, err, stCreated)
	mustNamespace(t, k8s, name)
	g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g.Status.Namespace = name
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(ctx, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func gatewaySpec(kinds ...laboratoryv1alpha1.GatewayKind) func(*laboratoryv1alpha1.LabSpec) {
	return func(s *laboratoryv1alpha1.LabSpec) {
		s.Gateways = nil
		for i, k := range kinds {
			s.Gateways = append(s.Gateways, laboratoryv1alpha1.GatewayTemplate{Name: string(k) + string(rune('a'+i)), Kind: k})
		}
	}
}

func createGroupLab(t *testing.T, h *Handler, group, name string, kinds ...laboratoryv1alpha1.GatewayKind) *protobuf.ItemResult {
	t.Helper()
	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: contractSpecJSON(t, gatewaySpec(kinds...), "web")}},
		Items:    []*protobuf.LabItem{{LabGroup: group, Name: name, VariantId: "v"}}})
	if err != nil {
		t.Fatalf("CreateLabs: %v", err)
	}
	return res.GetResults()[0]
}

// A new group has its servers OFF unless the item says otherwise; the switches are written on the object explicitly (the old
// vpn_disabled follows), the projection reports them, and a group made before the fields is reported ON.
func TestLabGroupSwitchesDefaultOffAndAreExplicit(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{
		{Name: "g-none"},
		{Name: "g-vpn", VpnEnabled: proto.Bool(true)},
		{Name: "g-net", InternetEnabled: proto.Bool(true)},
		{Name: "g-both", VpnEnabled: proto.Bool(true), InternetEnabled: proto.Bool(true)},
		{Name: "g-old", VpnDisabled: true, InternetEnabled: proto.Bool(true)},
	}})
	wantStates(t, res, err, stCreated, stCreated, stCreated, stCreated, stCreated)
	for name, want := range map[string][2]bool{"g-none": {false, false}, "g-vpn": {true, false}, "g-net": {false, true}, "g-both": {true, true}, "g-old": {false, true}} {
		g, err := groups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if g.Spec.VPN.Enabled == nil || g.Spec.Gateway.Enabled == nil {
			t.Fatalf("%s: the agent writes both switches explicitly", name)
		}
		if g.VPNEnabled() != want[0] || g.InternetEnabled() != want[1] || g.Spec.VPN.Disabled != !want[0] {
			t.Errorf("%s: vpn %v internet %v (disabled %v), want %v", name, g.VPNEnabled(), g.InternetEnabled(), g.Spec.VPN.Disabled, want)
		}
		if p := labGroupToProto(g); p.GetVpnEnabled() != want[0] || p.GetInternetEnabled() != want[1] {
			t.Errorf("%s: projection vpn %v internet %v", name, p.GetVpnEnabled(), p.GetInternetEnabled())
		}
	}

	// a repeat of the same item is EXISTS; the same name with other switches is a different spec
	res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g-vpn", VpnEnabled: proto.Bool(true)}}})
	wantStates(t, res, err, stExists)
	res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g-vpn", VpnEnabled: proto.Bool(true), InternetEnabled: proto.Bool(true)}}})
	wantStates(t, res, err, stFailed)

	// the new and the old switch must not contradict each other
	_, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g-bad", VpnEnabled: proto.Bool(true), VpnDisabled: true}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("vpn_enabled with vpn_disabled: %v", err)
	}
	_, err = h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{
		{Name: "g-vpn", Changes: &protobuf.LabGroupChanges{VpnEnabled: proto.Bool(true), VpnDisabled: proto.Bool(true)}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an update with both: %v", err)
	}

	// a group made before the switches (nothing set on the object) is reported ON, and the VPN follows the old disabled
	for name, spec := range map[string]laboratoryv1alpha1.LabGroupSpec{
		"g-legacy":          {},
		"g-legacy-disabled": {VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}},
	} {
		lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
		if _, err := groups.Create(ctx, lg, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		got, _ := groups.Get(ctx, name, metav1.GetOptions{})
		p := labGroupToProto(got)
		if p.GetVpnEnabled() != !spec.VPN.Disabled || !p.GetInternetEnabled() {
			t.Errorf("%s: a group made before the switches: vpn %v internet %v", name, p.GetVpnEnabled(), p.GetInternetEnabled())
		}
	}
}

// A lab whose topology has a connector is refused (stable reason) in a group whose server is off, and goes in after the switch is
// turned on; a lab without that connector is not touched.
func TestLabWithAConnectorIsRefusedWhileTheGroupServerIsOff(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	switchGroup(t, h, k8s, "g-off", &protobuf.LabGroupItem{})
	vpn, net := laboratoryv1alpha1.GatewayKindVPN, laboratoryv1alpha1.GatewayKindInternet

	for _, c := range []struct {
		lab    string
		kinds  []laboratoryv1alpha1.GatewayKind
		reason string
	}{
		{"l-vpn", []laboratoryv1alpha1.GatewayKind{vpn}, client.ReasonGroupVPNOff},
		{"l-net", []laboratoryv1alpha1.GatewayKind{net}, client.ReasonGroupInternetOff},
		{"l-both", []laboratoryv1alpha1.GatewayKind{net, vpn}, client.ReasonGroupVPNOff},
	} {
		wantReason(t, createGroupLab(t, h, "g-off", c.lab, c.kinds...), c.reason, false)
	}
	if r := createGroupLab(t, h, "g-off", "l-plain"); r.GetState() != stCreated {
		t.Fatalf("a lab with no connector needs no server: %v", r)
	}

	// turning the VPN on lets a VPN lab in, and still refuses an internet one
	res, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{{Name: "g-off", Changes: &protobuf.LabGroupChanges{VpnEnabled: proto.Bool(true)}}}})
	wantStates(t, res, err, stUpdated)
	if r := createGroupLab(t, h, "g-off", "l-vpn", vpn); r.GetState() != stCreated {
		t.Fatalf("VPN on: %v", r)
	}
	wantReason(t, createGroupLab(t, h, "g-off", "l-net", net), client.ReasonGroupInternetOff, false)
	res, err = h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{{Name: "g-off", Changes: &protobuf.LabGroupChanges{InternetEnabled: proto.Bool(true)}}}})
	wantStates(t, res, err, stUpdated)
	if r := createGroupLab(t, h, "g-off", "l-net", net); r.GetState() != stCreated {
		t.Fatalf("internet on: %v", r)
	}
}

// A server cannot be turned off while a lab of the group has a gateway that needs it; once the lab is gone it can.
func TestServerCannotBeTurnedOffWhileALabUsesIt(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	switchGroup(t, h, k8s, "g-on", &protobuf.LabGroupItem{VpnEnabled: proto.Bool(true), InternetEnabled: proto.Bool(true)})
	if r := createGroupLab(t, h, "g-on", "l-vpn", laboratoryv1alpha1.GatewayKindVPN); r.GetState() != stCreated {
		t.Fatal(r)
	}
	turnOff := func(c *protobuf.LabGroupChanges) *protobuf.ItemResult {
		res, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{{Name: "g-on", Changes: c}}})
		if err != nil {
			t.Fatal(err)
		}
		return res.GetResults()[0]
	}
	r := turnOff(&protobuf.LabGroupChanges{VpnEnabled: proto.Bool(false)})
	wantReason(t, r, client.ReasonGroupServerInUse, false)
	g, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "g-on", metav1.GetOptions{})
	if !g.VPNEnabled() {
		t.Fatal("a refused switch changes nothing")
	}
	// the old switch is the same switch
	wantReason(t, turnOff(&protobuf.LabGroupChanges{VpnDisabled: proto.Bool(true)}), client.ReasonGroupServerInUse, false)
	// the internet is not used by that lab: it can go off
	if r := turnOff(&protobuf.LabGroupChanges{InternetEnabled: proto.Bool(false)}); r.GetState() != stUpdated {
		t.Fatalf("internet off: %v", r)
	}
	if g, _ = h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "g-on", metav1.GetOptions{}); g.InternetEnabled() || !g.VPNEnabled() {
		t.Fatalf("only the internet went off: vpn %v internet %v", g.VPNEnabled(), g.InternetEnabled())
	}
	// the lab goes, and then the VPN can be turned off
	if err := h.cs.LaboratoryV1alpha1().Labs("g-on").Delete(ctx, crName("l-vpn"), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := turnOff(&protobuf.LabGroupChanges{VpnEnabled: proto.Bool(false)}); r.GetState() != stUpdated {
		t.Fatalf("vpn off after the lab is gone: %v", r)
	}
}

// A VPN client has no server to connect to while the group's VPN is off.
func TestClientIsRefusedWhileTheGroupVPNIsOff(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	switchGroup(t, h, k8s, "g-novpn", &protobuf.LabGroupItem{})
	res, err := h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{Items: []*protobuf.LabGroupClientItem{{LabGroup: "g-novpn", Name: "c1"}}})
	if err != nil {
		t.Fatal(err)
	}
	wantReason(t, res.GetResults()[0].GetResult(), client.ReasonGroupVPNOff, false)
	if _, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{Items: []*protobuf.UpdateLabGroupItem{{Name: "g-novpn", Changes: &protobuf.LabGroupChanges{VpnEnabled: proto.Bool(true)}}}}); err != nil {
		t.Fatal(err)
	}
	simulateClientReconciler(t, h, "g-novpn") // envtest has no operator: a goroutine plays it
	res, err = h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{Items: []*protobuf.LabGroupClientItem{{LabGroup: "g-novpn", Name: "c1"}}})
	if err != nil || res.GetResults()[0].GetResult().GetState() != stCreated {
		t.Fatalf("VPN on: %v %v", res, err)
	}
}
