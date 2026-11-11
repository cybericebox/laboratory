package grpc

import (
	"strings"
	"testing"
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
