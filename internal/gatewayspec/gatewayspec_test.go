package gatewayspec

import (
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func gw(name string, kind laboratoryv1alpha1.GatewayKind) laboratoryv1alpha1.GatewayTemplate {
	return laboratoryv1alpha1.GatewayTemplate{Name: name, Kind: kind}
}

func TestValidate(t *testing.T) {
	vpn, inet := laboratoryv1alpha1.GatewayKindVPN, laboratoryv1alpha1.GatewayKindInternet
	many := func(n int) []laboratoryv1alpha1.GatewayTemplate {
		var out []laboratoryv1alpha1.GatewayTemplate
		for i := 0; i < n; i++ {
			out = append(out, gw("g"+string(rune('a'+i)), vpn))
		}
		return out
	}
	for name, tc := range map[string]struct {
		spec  laboratoryv1alpha1.LabSpec
		limit int
		want  string // empty: valid
	}{
		"no gateways":                 {},
		"declared":                    {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{gw("attack", vpn), gw("out", inet)}}},
		"ten is the default limit":    {spec: laboratoryv1alpha1.LabSpec{Gateways: many(10)}},
		"eleven is over it":           {spec: laboratoryv1alpha1.LabSpec{Gateways: many(11)}, want: "TooManyGateways"},
		"a chart may raise the limit": {spec: laboratoryv1alpha1.LabSpec{Gateways: many(11)}, limit: 12},
		"a chart may lower it":        {spec: laboratoryv1alpha1.LabSpec{Gateways: many(3)}, limit: 2, want: "TooManyGateways"},
		"bad characters":              {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{gw("Attack_1", vpn)}}, want: "InvalidGatewayName"},
		"too long":                    {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{gw(strings.Repeat("a", 36), vpn)}}, want: "InvalidGatewayName"},
		"name of a device": {spec: laboratoryv1alpha1.LabSpec{
			Devices:  []laboratoryv1alpha1.DeviceTemplate{{Name: "attack"}},
			Gateways: []laboratoryv1alpha1.GatewayTemplate{gw("attack", vpn)},
		}, want: "DuplicateGatewayName"},
		"twice":              {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{gw("a", vpn), gw("a", inet)}}, want: "DuplicateGatewayName"},
		"unknown kind":       {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{gw("a", "tunnel")}}, want: "InvalidGateway"},
		"reverse on vpn":     {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{{Name: "a", Kind: vpn, ReverseConnections: true}}}},
		"reverse on inet":    {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{{Name: "a", Kind: inet, ReverseConnections: true}}}, want: "InvalidGateway"},
		"dhcp without range": {spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{{Name: "a", Kind: vpn, DHCPServer: &laboratoryv1alpha1.DHCPServer{Enabled: true}}}}, want: "InvalidGateway"},
	} {
		err := Validate(&tc.spec, tc.limit)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want prefix %q", name, err, tc.want)
		}
	}
}
