package grpc

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// What a lab spec may say about reverse connections, and that the removed vpn and internet keys are refused loudly.
func TestGatewaySpecsInTheLabSpec(t *testing.T) {
	for _, tc := range []struct {
		name, spec, want string
	}{
		{"a vpn gateway", `{"gateways":[{"name":"attack","kind":"vpn"}],"devices":[]}`, ""},
		{"reverse connections on a vpn gateway", `{"gateways":[{"name":"attack","kind":"vpn","reverseConnections":true}],"devices":[]}`, ""},
		{"the removed vpn key", `{"vpn":{"enabled":true},"devices":[]}`, `unknown field "vpn"`},
		{"the removed internet key", `{"internet":{"enabled":true},"devices":[]}`, `unknown field "internet"`},
		{"a reference by network", `{"devices":[{"name":"web","type":"container","image":"nginx","interfaces":[{"name":"eth0","addr":{"type":"static","addressRef":{"network":"vpn","host":10}}}]}]}`, `unknown field "network"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseLabSpec([]byte(tc.spec), false)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if spec.Gateways[0].ReverseConnections != strings.Contains(tc.spec, `"reverseConnections":true`) {
					t.Fatalf("value lost: %+v", spec.Gateways)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// The agent reports a gateway as blocked only when the gateway program confirmed it and the switch is off.
func TestLabStatusReportsTheBlockOnlyWhenConfirmed(t *testing.T) {
	mk := func(reverse, confirmed bool) *laboratoryv1alpha1.Lab {
		l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}}
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN, ReverseConnections: reverse}}
		l.Status.Gateways = []laboratoryv1alpha1.GatewayStatus{{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN, LabInitiatedBlocked: confirmed}}
		return l
	}
	for name, tc := range map[string]struct {
		lab  *laboratoryv1alpha1.Lab
		want bool
	}{
		"confirmed":                     {mk(false, true), true},
		"old gateway, not confirmed":    {mk(false, false), false},
		"switch on, stale confirmation": {mk(true, true), false},
	} {
		gws := labToProto(tc.lab).GetStatus().GetGateways()
		if len(gws) != 1 || gws[0].GetLabInitiatedBlocked() != tc.want {
			t.Errorf("%s: %v, want %v", name, gws, tc.want)
		}
	}
}
