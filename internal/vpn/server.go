//go:build linux

package vpn

import (
	"fmt"

	"github.com/cybericebox/laboratory/pkg/netutil"
)

// Server holds the running VPN server components.
type Server struct {
	WG  *WGManager
	IPT *IPTablesManager
}

// InitServer configures the WireGuard interface and iptables for the VPN server.
// Call Cleanup when done (deferred in main).
func InitServer(cfg *Config) (*Server, error) {
	wg, err := NewWGManager(cfg.WGInterface)
	if err != nil {
		return nil, fmt.Errorf("init WG manager: %w", err)
	}

	if err := wg.Init(cfg.PrivateKey, cfg.ListenPort); err != nil {
		wg.Close()
		return nil, fmt.Errorf("init WireGuard interface: %w", err)
	}

	// Assign client-subnet gateway IP to the WG interface so the kernel routes
	// client-subnet traffic via it (wgctrl only handles crypto/port).
	gwCIDR := netutil.FirstHostCIDR(cfg.ClientSubnet)
	if err := netutil.AssignIfaceIP(cfg.WGInterface, gwCIDR); err != nil {
		wg.Close()
		return nil, fmt.Errorf("assign gateway IP on %s: %w", cfg.WGInterface, err)
	}

	ipt, err := NewIPTablesManager(cfg.WGInterface)
	if err != nil {
		wg.Close()
		return nil, fmt.Errorf("init iptables: %w", err)
	}
	if err := ipt.SetupForwardPolicy(); err != nil {
		wg.Close()
		return nil, fmt.Errorf("setup FORWARD policy: %w", err)
	}

	return &Server{WG: wg, IPT: ipt}, nil
}

// Cleanup shuts down the WireGuard interface and removes iptables rules.
func (s *Server) Cleanup() {
	s.WG.Close()
	s.IPT.Cleanup()
}
