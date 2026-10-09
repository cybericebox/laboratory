package dhcp

import (
	"fmt"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func serverIP(cfg Config) net.IP {
	if cfg.BindIP != "" {
		return net.ParseIP(cfg.BindIP).To4()
	}
	return net.ParseIP(cfg.Gateway).To4()
}
func nonzero(ip net.IP) bool { return ip != nil && ip.To4() != nil && !ip.Equal(net.IPv4zero) }
func (p *ipPool) owns(mac net.HardwareAddr, ip net.IP, committed bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge()
	r, ok := p.byMAC[mac.String()]
	return ok && r.ip.Equal(ip) && (!committed || r.committed)
}

// replyForMessage follows RFC 2131 state distinctions before touching leases.
func replyForMessage(cfg Config, pool *ipPool, msg *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	if err := validateClientMessage(msg); err != nil {
		return nil, err
	}
	sid := serverIP(cfg)
	if sid == nil {
		return nil, fmt.Errorf("invalid server identifier")
	}
	selected := msg.ServerIdentifier()
	if selected != nil && !selected.Equal(sid) {
		return nil, nil
	}
	kind := msg.MessageType()
	var assigned net.IP
	var err error
	replyKind := dhcpv4.MessageTypeAck
	leaseReply := false
	switch kind {
	case dhcpv4.MessageTypeDiscover:
		assigned, err = pool.Offer(msg.ClientHWAddr)
		if err != nil {
			return nil, err
		}
		replyKind = dhcpv4.MessageTypeOffer
		leaseReply = true
	case dhcpv4.MessageTypeRequest:
		requested := msg.RequestedIPAddress()
		if nonzero(msg.ClientIPAddr) {
			if selected != nil || requested != nil {
				return nil, fmt.Errorf("invalid renewal options")
			}
			requested = msg.ClientIPAddr
			if !pool.owns(msg.ClientHWAddr, requested, true) {
				replyKind = dhcpv4.MessageTypeNak
			}
		} else if !nonzero(requested) {
			return nil, fmt.Errorf("missing requested address")
		}
		if selected != nil && !pool.owns(msg.ClientHWAddr, requested, false) {
			replyKind = dhcpv4.MessageTypeNak
		}
		if replyKind != dhcpv4.MessageTypeNak {
			assigned, err = pool.Commit(msg.ClientHWAddr, requested)
			if err != nil {
				replyKind = dhcpv4.MessageTypeNak
			} else {
				leaseReply = true
			}
		}
	case dhcpv4.MessageTypeRelease:
		if selected == nil || !nonzero(msg.ClientIPAddr) {
			return nil, fmt.Errorf("invalid RELEASE")
		}
		pool.Release(msg.ClientHWAddr, msg.ClientIPAddr)
		return nil, nil
	case dhcpv4.MessageTypeDecline:
		if selected == nil || !nonzero(msg.RequestedIPAddress()) {
			return nil, fmt.Errorf("invalid DECLINE")
		}
		pool.Decline(msg.ClientHWAddr, msg.RequestedIPAddress())
		return nil, nil
	case dhcpv4.MessageTypeInform:
		// No yiaddr or lease duration in configuration-only ACK.
	default:
		return nil, nil
	}
	opts := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(replyKind),
		dhcpv4.WithServerIP(sid),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(sid)),
	}
	if replyKind != dhcpv4.MessageTypeNak {
		_, subnet, err := net.ParseCIDR(cfg.Subnet)
		if err != nil {
			return nil, err
		}
		opts = append(opts, dhcpv4.WithNetmask(subnet.Mask), dhcpv4.WithRouter(net.ParseIP(cfg.Gateway)))
		if cfg.DNS != "" {
			ip := net.ParseIP(cfg.DNS).To4()
			if ip == nil {
				return nil, fmt.Errorf("invalid DNS")
			}
			opts = append(opts, dhcpv4.WithDNS(ip))
		}
		if leaseReply {
			opts = append(opts, dhcpv4.WithYourIP(assigned), dhcpv4.WithLeaseTime(uint32(leaseDuration.Seconds())))
		}
	}
	return dhcpv4.NewReplyFromRequest(msg, opts...)
}

func validateClientMessage(msg *dhcpv4.DHCPv4) error {
	if msg == nil || msg.OpCode != dhcpv4.OpcodeBootRequest || !validMAC(msg.ClientHWAddr) {
		return fmt.Errorf("invalid DHCP client identity")
	}
	for _, code := range []dhcpv4.OptionCode{dhcpv4.OptionServerIdentifier, dhcpv4.OptionRequestedIPAddress} {
		if raw := msg.Options.Get(code); raw != nil && len(raw) != 4 {
			return fmt.Errorf("malformed address option")
		}
	}
	if len(msg.Options.Get(dhcpv4.OptionDHCPMessageType)) != 1 {
		return fmt.Errorf("malformed DHCP message type")
	}
	return nil
}
