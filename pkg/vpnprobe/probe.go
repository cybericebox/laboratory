// Package vpnprobe defines the address of the tunnel-only connection check.
package vpnprobe

import (
	"fmt"
	"net"
)

const Port = 8088

// GatewayIP returns the first usable IPv4 address of the group's client
// subnet. The VPN process binds its check page to this exact address.
func GatewayIP(cidr string) (string, error) {
	ip, subnet, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return "", fmt.Errorf("invalid IPv4 client subnet %q", cidr)
	}
	base := subnet.IP.To4()
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones > 30 || ones < 1 {
		return "", fmt.Errorf("client subnet %q has no usable gateway", cidr)
	}
	gw := append(net.IP(nil), base...)
	for i := len(gw) - 1; i >= 0; i-- {
		gw[i]++
		if gw[i] != 0 {
			break
		}
	}
	return gw.String(), nil
}
