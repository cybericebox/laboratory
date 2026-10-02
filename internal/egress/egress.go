package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// DefaultDenyCIDRs are the destinations a lab never reaches through its internet gateway: link-local (the cloud
// metadata service), loopback, the private ranges (RFC 1918, so the node, VPC, pod and service networks of a
// typical cluster), CGNAT, the benchmark and protocol-assignment ranges, and multicast/reserved. Only the public
// internet is left. Mirrors the chart (inetGateway.egress.denyCIDRs).
const DefaultDenyCIDRs = "169.254.0.0/16,127.0.0.0/8,0.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,192.0.0.0/24,198.18.0.0/15,224.0.0.0/3"

// NormalizeCIDRs checks every entry and returns its canonical form (masked address and length).
func NormalizeCIDRs(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, c := range list {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("%q is not an IPv4 CIDR", c)
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}
