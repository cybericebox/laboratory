//go:build linux

package vpn

import (
	"fmt"
	"log"

	"github.com/cybericebox/laboratory/pkg/netutil"
	"github.com/cybericebox/laboratory/pkg/vpnprobe"
)

// Server holds the running VPN server components.
type Server struct {
	WG    *WGManager
	IPT   *IPTablesManager
	Probe *probeServer
}

// InitServer configures the WireGuard interface and iptables for the VPN server.
// Call Cleanup when done (deferred in main).
func InitServer(cfg *Config) (*Server, error) {
	// The INPUT rules come first: nothing on the lab side may talk to the pod, and the WireGuard port must not be open to it even
	// for a moment. The status page address is known from the client subnet before the interface exists.
	probeAddr, err := vpnprobe.GatewayIP(cfg.ClientSubnet.String())
	if err != nil {
		return nil, fmt.Errorf("VPN gateway address: %w", err)
	}
	ipt, err := NewIPTablesManager(cfg.WGInterface)
	if err != nil {
		return nil, fmt.Errorf("init iptables: %w", err)
	}
	ipv6Covered, err := ipt.ProtectInput(cfg.ExternalInterface, probeAddr, ProbePort)
	if err != nil {
		ipt.Cleanup()
		return nil, fmt.Errorf("install the INPUT policy: %w", err)
	}
	if !ipv6Covered {
		log.Printf("no ip6tables in this pod: the INPUT policy covers IPv4 only")
	}
	ipv6Guarded, err := ipt.GuardWireGuardPort(cfg.ExternalInterface, cfg.ListenPort)
	if err != nil {
		ipt.Cleanup()
		return nil, fmt.Errorf("guard the WireGuard port %d: %w", cfg.ListenPort, err)
	}
	if !ipv6Guarded {
		log.Printf("no ip6tables in this pod: the WireGuard port is guarded for IPv4 only")
	}

	wg, err := NewWGManager(cfg.WGInterface)
	if err != nil {
		ipt.Cleanup()
		return nil, fmt.Errorf("init WG manager: %w", err)
	}

	if err := wg.Init(cfg.PrivateKey, cfg.ListenPort); err != nil {
		wg.Close()
		ipt.Cleanup()
		return nil, fmt.Errorf("init WireGuard interface: %w", err)
	}

	// Assign client-subnet gateway IP to the WG interface so the kernel routes
	// client-subnet traffic via it (wgctrl only handles crypto/port).
	gwCIDR := netutil.FirstHostCIDR(cfg.ClientSubnet)
	if err := netutil.AssignIfaceIP(cfg.WGInterface, gwCIDR); err != nil {
		wg.Close()
		ipt.Cleanup()
		return nil, fmt.Errorf("assign gateway IP on %s: %w", cfg.WGInterface, err)
	}

	if err := ipt.SetupForwardPolicy(); err != nil {
		wg.Close()
		ipt.Cleanup()
		return nil, fmt.Errorf("setup FORWARD policy: %w", err)
	}
	probe, err := startProbe(cfg.ClientSubnet, ProbePort, cfg.SupportEmail, cfg.WGInterface)
	if err != nil {
		ipt.Cleanup()
		wg.Close()
		return nil, fmt.Errorf("start VPN gateway probe: %w", err)
	}

	return &Server{WG: wg, IPT: ipt, Probe: probe}, nil
}

// Cleanup shuts down the WireGuard interface and removes iptables rules.
func (s *Server) Cleanup() {
	s.Probe.Close()
	s.WG.Close()
	s.IPT.Cleanup()
}
