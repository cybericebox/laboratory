package egress

import (
	"net/netip"
	"testing"
)

func contains(list []string, ip string) bool {
	a := netip.MustParseAddr(ip)
	for _, c := range list {
		if netip.MustParsePrefix(c).Contains(a) {
			return true
		}
	}
	return false
}

// The metadata service, loopback and every private range are denied; public addresses are not.
func TestDeniedAndAllowed(t *testing.T) {
	for _, ip := range []string{"169.254.169.254", "127.0.0.1", "10.1.2.3", "172.31.0.5", "192.168.1.1", "100.64.0.1", "0.0.0.1", "224.0.0.5", "255.255.255.255", "198.19.0.1"} {
		if !contains(DenyV4, ip) {
			t.Errorf("%s must be denied", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.32.0.1", "100.128.0.1", "192.0.2.1"} {
		if contains(DenyV4, ip) {
			t.Errorf("%s is public and must be allowed", ip)
		}
	}
	for _, ip := range []string{"::1", "fe80::1", "fd00::1", "fc00::1", "ff02::1", "::ffff:10.0.0.1", "64:ff9b::a00:1"} {
		if !contains(DenyV6, ip) {
			t.Errorf("%s must be denied", ip)
		}
	}
	for _, ip := range []string{"2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if contains(DenyV6, ip) {
			t.Errorf("%s is public and must be allowed", ip)
		}
	}
}

func TestListsAreValidPrefixes(t *testing.T) {
	for _, c := range append(append([]string{}, DenyV4...), DenyV6...) {
		p, err := netip.ParsePrefix(c)
		if err != nil || p.Masked() != p {
			t.Errorf("%q is not a canonical prefix: %v", c, err)
		}
	}
}
