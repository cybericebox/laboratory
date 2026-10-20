package laboratory

import (
	"net/netip"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/netutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// vpnClientSubnet is the subnet of the participants' WireGuard clients: the first /24 of the VPN base network. Empty base: none
// (the lab then gets no return route).
func vpnClientSubnet(base string) (netip.Prefix, error) {
	if base == "" {
		return netip.Prefix{}, nil
	}
	cidr, err := netutil.SubnetForIndex(base, labSubnetPrefixLen, 0)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.ParsePrefix(cidr)
}

// addVPNReturnRoutes gives every static interface connected to the lab's VPN network the route back to the participants: the
// client subnet via the VPN gateway (host .1 of the lab's VPN network). Without it the replies of the device have no way to a
// client and the lab is unreachable. An interface that already routes the client subnet (a route of its own that covers it, or
// the VPN gateway as its default gateway) is left as the author wrote it. It changes the resolved interfaces only, never the
// Lab template.
func addVPNReturnRoutes(lab *laboratoryv1alpha1.Lab, resolved map[string][]laboratoryv1alpha1.InterfaceSpec, clients netip.Prefix) {
	if !clients.IsValid() || !lab.Spec.VPN.Enabled {
		return
	}
	vpn, err := resolveNetworkPrefix(lab, names.ComponentVPN)
	if err != nil {
		return
	}
	gateway := netip.AddrFrom4([4]byte{vpn.Addr().As4()[0], vpn.Addr().As4()[1], vpn.Addr().As4()[2], 1})
	attached := vpnAttachedInterfaces(lab)
	for device, interfaces := range resolved {
		for i := range interfaces {
			iface := &interfaces[i]
			if iface.Addr == nil || iface.Addr.Type != laboratoryv1alpha1.AddrTypeStatic || !attached[device+"/"+iface.Name] {
				continue
			}
			own, err := netip.ParsePrefix(iface.Addr.IP)
			// The gateway must be on the link of the interface, and not the interface itself.
			if err != nil || !own.Contains(gateway) || own.Addr() == gateway {
				continue
			}
			if iface.Addr.Gateway == gateway.String() || routesCover(iface.Addr.Routes, clients) {
				continue
			}
			iface.Addr.Routes = append(iface.Addr.Routes, laboratoryv1alpha1.Route{Dst: clients.String(), Via: gateway.String()})
		}
	}
}

func routesCover(routes []laboratoryv1alpha1.Route, target netip.Prefix) bool {
	for _, route := range routes {
		dst, err := netip.ParsePrefix(route.Dst)
		if err == nil && dst.Bits() <= target.Bits() && dst.Contains(target.Addr()) {
			return true
		}
	}
	return false
}

// vpnAttachedInterfaces is the set of "device/interface" connected to the VPN network: directly, or through switches and hubs.
func vpnAttachedInterfaces(lab *laboratoryv1alpha1.Lab) map[string]bool {
	switches := map[string]bool{}
	for _, d := range lab.Spec.Devices {
		if d.Type == laboratoryv1alpha1.DeviceTypeUnmanagedSwitch || d.Type == laboratoryv1alpha1.DeviceTypeHub {
			switches[d.Name] = true
		}
	}
	// A switch is on the VPN when it is connected to it or to a switch that is.
	onVPN := map[string]bool{names.ComponentVPN: true}
	for changed := true; changed; {
		changed = false
		for _, c := range lab.Spec.Connections {
			if len(c.Endpoints) != 2 {
				continue
			}
			a, b := c.Endpoints[0].Device, c.Endpoints[1].Device
			if switches[a] && onVPN[b] && !onVPN[a] {
				onVPN[a], changed = true, true
			}
			if switches[b] && onVPN[a] && !onVPN[b] {
				onVPN[b], changed = true, true
			}
		}
	}
	attached := map[string]bool{}
	for _, c := range lab.Spec.Connections {
		if len(c.Endpoints) != 2 {
			continue
		}
		for n, other := range [2]int{1, 0} {
			end, peer := c.Endpoints[n], c.Endpoints[other]
			if !switches[end.Device] && end.Device != names.ComponentVPN && onVPN[peer.Device] {
				attached[end.Device+"/"+end.Interface] = true
			}
		}
	}
	return attached
}
