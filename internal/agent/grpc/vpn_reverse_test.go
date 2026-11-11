package grpc

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestReverseConnectionsSwitchOfTheVPNSegment(t *testing.T) {
	for _, tc := range []struct {
		name, spec, want string
	}{
		{"default is off", `{"vpn":{"enabled":true},"devices":[]}`, ""},
		{"on with a vpn", `{"vpn":{"enabled":true,"reverseConnections":true},"devices":[]}`, ""},
		{"explicit off without a vpn", `{"vpn":{"reverseConnections":false},"devices":[]}`, ""},
		{"on without a vpn", `{"vpn":{"reverseConnections":true},"devices":[]}`, "needs vpn.enabled"},
		{"on with the vpn off", `{"vpn":{"enabled":false,"reverseConnections":true},"devices":[]}`, "needs vpn.enabled"},
		{"on the internet segment", `{"vpn":{"enabled":true},"internet":{"enabled":true,"reverseConnections":true},"devices":[]}`, "vpn segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseLabSpec([]byte(tc.spec), false)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if spec.VPN.ReverseConnections != strings.Contains(tc.spec, `"reverseConnections":true`) {
					t.Fatalf("value lost: %+v", spec.VPN)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// The agent reports a lab as blocked only when the gateway confirmed it and the switch is off.
func TestLabStatusReportsTheBlockOnlyWhenConfirmed(t *testing.T) {
	mk := func(enabled, reverse, confirmed bool) *laboratoryv1alpha1.Lab {
		l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}}
		l.Spec.VPN.Enabled, l.Spec.VPN.ReverseConnections = enabled, reverse
		l.Status.VPN.LabInitiatedBlocked = confirmed
		return l
	}
	for name, tc := range map[string]struct {
		lab  *laboratoryv1alpha1.Lab
		want bool
	}{
		"confirmed":                     {mk(true, false, true), true},
		"old gateway, not confirmed":    {mk(true, false, false), false},
		"switch on, stale confirmation": {mk(true, true, true), false},
	} {
		if got := labToProto(tc.lab).GetStatus().GetVpnLabInitiatedBlocked(); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
