package names

import "testing"

func TestWithPublicVPNPort(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "",
		"vpn.example.com":     "vpn.example.com:51820",
		"vpn.example.com:443": "vpn.example.com:443",
		"203.0.113.7":         "203.0.113.7:51820",
		"203.0.113.7:51820":   "203.0.113.7:51820",
		"[2001:db8::1]":       "[2001:db8::1]:51820",
		"[2001:db8::1]:5555":  "[2001:db8::1]:5555",
	} {
		if got := WithPublicVPNPort(in); got != want {
			t.Errorf("WithPublicVPNPort(%q) = %q, want %q", in, got, want)
		}
	}
}
