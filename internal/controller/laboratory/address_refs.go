package laboratory

import (
	"fmt"
	"net/netip"

	"github.com/cybericebox/laboratory/internal/names"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// resolveLabDeviceInterfaces validates every reference before reconciliation
// creates any Device. Lab.Spec remains a reusable template for later runs.
func resolveLabDeviceInterfaces(lab *laboratoryv1alpha1.Lab) (map[string][]laboratoryv1alpha1.InterfaceSpec, error) {
	resolved := make(map[string][]laboratoryv1alpha1.InterfaceSpec, len(lab.Spec.Devices))
	usedAddresses := make(map[laboratoryv1alpha1.NetworkIPRef]string)
	for _, tmpl := range lab.Spec.Devices {
		for _, iface := range tmpl.Interfaces {
			if ref := iface.Addr.AddressRef; ref != nil {
				if previous, exists := usedAddresses[*ref]; exists {
					return nil, fmt.Errorf("duplicate static address %s host %d on %s and %s.%s", ref.Network, ref.Host, previous, tmpl.Name, iface.Name)
				}
				usedAddresses[*ref] = tmpl.Name + "." + iface.Name
			}
		}
		interfaces, err := resolveDeviceInterfaces(tmpl, lab)
		if err != nil {
			return nil, fmt.Errorf("device %q: %w", tmpl.Name, err)
		}
		resolved[tmpl.Name] = interfaces
	}
	return resolved, nil
}

// resolveDeviceInterfaces converts typed subnet references to concrete strings
// for one lab allocation, leaving both the Lab template and literal values intact.
func resolveDeviceInterfaces(tmpl laboratoryv1alpha1.DeviceTemplate, lab *laboratoryv1alpha1.Lab) ([]laboratoryv1alpha1.InterfaceSpec, error) {
	resolved := make([]laboratoryv1alpha1.InterfaceSpec, len(tmpl.Interfaces))
	for index, iface := range tmpl.Interfaces {
		out := iface
		addr := iface.Addr
		if addr.Type != laboratoryv1alpha1.AddrTypeStatic {
			if addr.IP != "" || addr.AddressRef != nil || addr.Gateway != "" || addr.GatewayRef != nil || len(addr.Routes) != 0 {
				return nil, fmt.Errorf("interface %q: non-static mode contains static address values", iface.Name)
			}
			resolved[index] = out
			continue
		}

		if (addr.IP == "") == (addr.AddressRef == nil) {
			return nil, fmt.Errorf("interface %q: require exactly one literal or referenced address", iface.Name)
		}
		if addr.AddressRef != nil {
			ip, err := resolveNetworkIP(lab, *addr.AddressRef, true)
			if err != nil {
				return nil, fmt.Errorf("interface %q address: %w", iface.Name, err)
			}
			addr.IP = ip + "/24"
			addr.AddressRef = nil
		}
		addressPrefix, err := netip.ParsePrefix(addr.IP)
		if err != nil {
			return nil, fmt.Errorf("interface %q: invalid address %q: %w", iface.Name, addr.IP, err)
		}

		if addr.Gateway != "" && addr.GatewayRef != nil {
			return nil, fmt.Errorf("interface %q: gateway has both literal and reference", iface.Name)
		}
		if addr.GatewayRef != nil {
			addr.Gateway, err = resolveNetworkIP(lab, *addr.GatewayRef, false)
			if err != nil {
				return nil, fmt.Errorf("interface %q gateway: %w", iface.Name, err)
			}
			addr.GatewayRef = nil
		}
		if addr.Gateway != "" {
			gateway, parseErr := netip.ParseAddr(addr.Gateway)
			if parseErr != nil || gateway.Is4() != addressPrefix.Addr().Is4() {
				return nil, fmt.Errorf("interface %q: gateway %q has invalid address family", iface.Name, addr.Gateway)
			}
		}

		addr.Routes = make([]laboratoryv1alpha1.Route, len(iface.Addr.Routes))
		for routeIndex, route := range iface.Addr.Routes {
			if (route.Dst == "") == (route.DstRef == nil) || (route.Via == "") == (route.ViaRef == nil) {
				return nil, fmt.Errorf("interface %q route %d: require one destination and one next hop", iface.Name, routeIndex)
			}
			if route.DstRef != nil {
				prefix, resolveErr := resolveNetworkPrefix(lab, route.DstRef.Network)
				if resolveErr != nil {
					return nil, fmt.Errorf("interface %q route %d destination: %w", iface.Name, routeIndex, resolveErr)
				}
				route.Dst = prefix.String()
				route.DstRef = nil
			}
			if route.ViaRef != nil {
				route.Via, err = resolveNetworkIP(lab, *route.ViaRef, false)
				if err != nil {
					return nil, fmt.Errorf("interface %q route %d next hop: %w", iface.Name, routeIndex, err)
				}
				route.ViaRef = nil
			}
			destination, dstErr := netip.ParsePrefix(route.Dst)
			nextHop, viaErr := netip.ParseAddr(route.Via)
			if dstErr != nil || viaErr != nil || destination.Addr().Is4() != nextHop.Is4() {
				return nil, fmt.Errorf("interface %q route %d: destination and next hop must use the same address family", iface.Name, routeIndex)
			}
			addr.Routes[routeIndex] = route
		}
		out.Addr = addr
		resolved[index] = out
	}
	return resolved, nil
}

func resolveNetworkIP(lab *laboratoryv1alpha1.Lab, ref laboratoryv1alpha1.NetworkIPRef, interfaceAddress bool) (string, error) {
	minimum := int32(1)
	if interfaceAddress {
		minimum = 2
	}
	if ref.Host < minimum || ref.Host > 254 {
		return "", fmt.Errorf("host %d outside %d..254", ref.Host, minimum)
	}
	prefix, err := resolveNetworkPrefix(lab, ref.Network)
	if err != nil {
		return "", err
	}
	ip := prefix.Addr().As4()
	ip[3] = byte(ref.Host)
	return netip.AddrFrom4(ip).String(), nil
}

func resolveNetworkPrefix(lab *laboratoryv1alpha1.Lab, network string) (netip.Prefix, error) {
	var enabled bool
	var cidr string
	switch network {
	case names.ComponentVPN:
		enabled, cidr = lab.Spec.VPN.Enabled, lab.Status.VPN.CIDR
	case "internet":
		enabled, cidr = lab.Spec.Internet.Enabled, lab.Status.Internet.CIDR
	default:
		return netip.Prefix{}, fmt.Errorf("unknown network %q", network)
	}
	if !enabled {
		return netip.Prefix{}, fmt.Errorf("%s network is disabled", network)
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 || prefix != prefix.Masked() {
		return netip.Prefix{}, fmt.Errorf("%s allocated CIDR %q is not a canonical IPv4 /24", network, cidr)
	}
	return prefix, nil
}
