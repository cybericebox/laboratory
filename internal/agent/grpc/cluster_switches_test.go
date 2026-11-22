package grpc

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/switches"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func offFeatures(mod func(*switches.Cluster)) Features {
	f := testFeatures
	sw := switches.Everything()
	mod(&sw)
	f.Switches = &sw
	return f
}

// switchedHandler is a handler with a ready group made while the laboratory served everything, which then turns the given features off
// (the order of a chart upgrade).
func switchedHandler(t *testing.T, f Features) *Handler {
	t.Helper()
	h, _ := contractHandler(t, testFeatures)
	h.SetFeatures(f)
	return h
}

// GetFeatures tells the truth about the chart switches.
func TestFeaturesReportClusterSwitches(t *testing.T) {
	for _, c := range []struct {
		name                          string
		mod                           func(*switches.Cluster)
		vpn, internet, http, rdp, vnc bool
	}{
		{"all on", func(*switches.Cluster) {}, true, true, true, true, true},
		{"vpn off", func(s *switches.Cluster) { s.VPN = false }, false, true, true, true, true},
		{"internet off", func(s *switches.Cluster) { s.Internet = false }, true, false, true, true, true},
		{"proxy off", func(s *switches.Cluster) { s.L7 = false }, true, true, false, false, false},
		{"rdp and vnc off", func(s *switches.Cluster) { s.RDP, s.VNC = false, false }, true, true, true, false, false},
	} {
		h := featuresHandler(t, offFeatures(c.mod), newTenantTenant("a", true, nil))
		got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		a := got.GetAccess()
		if got.GetVpn().GetEnabled() != c.vpn || got.GetInternet().GetEnabled() != c.internet || a.GetHttp() != c.http || a.GetRdp() != c.rdp || a.GetVnc() != c.vnc {
			t.Errorf("%s: vpn %v internet %v access %+v", c.name, got.GetVpn().GetEnabled(), got.GetInternet().GetEnabled(), a)
		}
	}
}

func refusedWith(t *testing.T, res *protobuf.BatchResult, err error, reason string) {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != stFailed || res.GetResults()[0].GetReason() != reason {
		t.Fatalf("want a refusal %s, got %v", reason, res.GetResults())
	}
}

// A lab that needs a feature the laboratory does not serve is refused with a stable reason.
func TestCreateLabsRefusedByClusterSwitches(t *testing.T) {
	gw := func(k laboratoryv1alpha1.GatewayKind) func(*laboratoryv1alpha1.LabSpec) {
		return func(s *laboratoryv1alpha1.LabSpec) {
			s.Gateways = []laboratoryv1alpha1.GatewayTemplate{{Name: "g", Kind: k}}
		}
	}
	access := func(k laboratoryv1alpha1.AccessKind) func(*laboratoryv1alpha1.LabSpec) {
		return func(s *laboratoryv1alpha1.LabSpec) {
			a := laboratoryv1alpha1.DeviceAccess{Kind: k}
			if k != laboratoryv1alpha1.AccessTerminal {
				a.Port = 8080
			}
			s.Devices[0].Access = []laboratoryv1alpha1.DeviceAccess{a}
		}
	}
	for _, c := range []struct {
		name   string
		off    func(*switches.Cluster)
		spec   func(*laboratoryv1alpha1.LabSpec)
		reason string
	}{
		{"vpn", func(s *switches.Cluster) { s.VPN = false }, gw(laboratoryv1alpha1.GatewayKindVPN), client.ReasonVPNUnavailable},
		{"internet", func(s *switches.Cluster) { s.Internet = false }, gw(laboratoryv1alpha1.GatewayKindInternet), client.ReasonInternetUnavailable},
		{"http without the proxy", func(s *switches.Cluster) { s.L7 = false }, access(laboratoryv1alpha1.AccessHTTP), client.ReasonAccessUnavailable},
		{"rdp", func(s *switches.Cluster) { s.RDP = false }, access(laboratoryv1alpha1.AccessRDP), client.ReasonAccessUnavailable},
		{"vnc", func(s *switches.Cluster) { s.VNC = false }, access(laboratoryv1alpha1.AccessVNC), client.ReasonAccessUnavailable},
		{"console port", func(s *switches.Cluster) { s.ConsoleCables = false }, func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].ConsolePorts = []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}
		}, client.ReasonConsoleCablesUnavailable},
		{"serial port", func(s *switches.Cluster) { s.ConsoleCables = false }, func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
		}, client.ReasonConsoleCablesUnavailable},
	} {
		h := switchedHandler(t, offFeatures(c.off))
		res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, c.spec, "web")}, &protobuf.LabItem{})
		refusedWith(t, res, nil, c.reason)
	}
	// the same specs pass while the laboratory serves everything
	for _, spec := range []func(*laboratoryv1alpha1.LabSpec){gw(laboratoryv1alpha1.GatewayKindVPN), gw(laboratoryv1alpha1.GatewayKindInternet), access(laboratoryv1alpha1.AccessHTTP)} {
		h := switchedHandler(t, offFeatures(func(*switches.Cluster) {}))
		wantStates(t, createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, spec, "web")}, &protobuf.LabItem{}), nil, stCreated)
	}
}

// Clients need the VPN; a policy is read by the VPN or the L7 proxy, so it is refused only when both are off.
func TestClientsAndPoliciesRefusedByClusterSwitches(t *testing.T) {
	ctx := t.Context()
	newClient := func(h *Handler) (*protobuf.BatchResult, error) {
		out, err := h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{Items: []*protobuf.LabGroupClientItem{{LabGroup: "team-lab", Name: "alice"}}})
		if err != nil {
			return nil, err
		}
		return &protobuf.BatchResult{Results: []*protobuf.ItemResult{out.GetResults()[0].GetResult()}}, nil
	}
	policy := func(h *Handler) (*protobuf.BatchResult, error) {
		return h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "team-lab"}}})
	}
	h := switchedHandler(t, offFeatures(func(s *switches.Cluster) { s.VPN = false }))
	res, err := newClient(h)
	refusedWith(t, res, err, client.ReasonVPNUnavailable)
	res, err = policy(h) // the L7 proxy still reads it
	if err != nil || res.GetResults()[0].GetReason() == client.ReasonVPNUnavailable {
		t.Fatalf("policy with the proxy on: %v %v", res, err)
	}
	h = switchedHandler(t, offFeatures(func(s *switches.Cluster) { s.VPN, s.L7 = false, false }))
	res, err = policy(h)
	refusedWith(t, res, err, client.ReasonVPNUnavailable)
}

// A group cannot turn on a server the laboratory does not run, in either shape of the item.
func TestCreateLabGroupsRefusedByClusterSwitches(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetFeatures(offFeatures(func(s *switches.Cluster) { s.VPN, s.Internet = false, false }))
	on, off := true, false
	for _, c := range []struct {
		name   string
		item   *protobuf.LabGroupItem
		reason string
	}{
		{"vpn on", &protobuf.LabGroupItem{Name: "g1", VpnEnabled: &on, InternetEnabled: &off}, client.ReasonVPNUnavailable},
		{"internet on", &protobuf.LabGroupItem{Name: "g2", VpnEnabled: &off, InternetEnabled: &on}, client.ReasonInternetUnavailable},
	} {
		res, err := h.CreateLabGroups(t.Context(), &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{c.item}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := res.GetResults(); len(got) != 1 || got[0].GetState() == stCreated || got[0].GetReason() != c.reason {
			t.Errorf("%s: want a refusal %s, got %v", c.name, c.reason, got)
		}
	}
	// an item in the old shape (neither field) is not refused: it just gets the servers the laboratory does not run turned off
	res, err := h.CreateLabGroups(t.Context(), &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g3"}}})
	wantStates(t, res, err, stCreated)
	if g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(t.Context(), "g3", metav1.GetOptions{}); err != nil || g.VPNEnabled() || g.InternetEnabled() {
		t.Fatalf("legacy item on a cluster without them: %+v %v", g, err)
	}
	res, err = h.CreateLabGroups(t.Context(), &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g4", VpnEnabled: &off, InternetEnabled: &off}}})
	wantStates(t, res, err, stCreated)
}
