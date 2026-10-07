package dhcp

import (
	"github.com/insomniacslk/dhcp/dhcpv4"
	"net"
	"testing"
	"time"
)

func handlerConfig() Config {
	return Config{Iface: "lab1", Subnet: "10.9.4.0/24", Gateway: "10.9.4.1", DNS: "1.1.1.1", Ranges: []Range{{2, 2}}}
}
func message(t *testing.T, kind dhcpv4.MessageType, mac net.HardwareAddr, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	mods = append([]dhcpv4.Modifier{dhcpv4.WithMessageType(kind)}, mods...)
	m, e := dhcpv4.NewDiscovery(mac, mods...)
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestReplyForMessageLeaseLifecycle(t *testing.T) {
	cfg := handlerConfig()
	p := smallPool()
	sid := dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(cfg.Gateway)))
	offer, e := replyForMessage(cfg, p, message(t, dhcpv4.MessageTypeDiscover, macA))
	if e != nil || offer == nil || offer.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("offer %v %v", offer, e)
	}
	ip := offer.YourIPAddr
	request := message(t, dhcpv4.MessageTypeRequest, macA, sid, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(ip)))
	ack, e := replyForMessage(cfg, p, request)
	if e != nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("ack %v %v", ack, e)
	}
	decoded, e := dhcpv4.FromBytes(ack.ToBytes())
	if e != nil || !decoded.ServerIdentifier().Equal(net.ParseIP(cfg.Gateway)) || len(decoded.Router()) != 1 || len(decoded.DNS()) != 1 || decoded.IPAddressLeaseTime(-1) != leaseDuration {
		t.Fatalf("wire options %v %v", decoded, e)
	}
	renew := message(t, dhcpv4.MessageTypeRequest, macA, dhcpv4.WithClientIP(ip))
	if a, e := replyForMessage(cfg, p, renew); e != nil || a.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("renew %v %v", a, e)
	}
	_, e = replyForMessage(cfg, p, message(t, dhcpv4.MessageTypeRelease, macB, sid, dhcpv4.WithClientIP(ip)))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Allocate(macB); e == nil {
		t.Fatal("foreign release freed lease")
	}
	_, e = replyForMessage(cfg, p, message(t, dhcpv4.MessageTypeRelease, macA, sid, dhcpv4.WithClientIP(ip)))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Allocate(macB); e != nil {
		t.Fatal(e)
	}
}
func TestRequestOtherServerAndInformDoNotAllocate(t *testing.T) {
	for _, kind := range []dhcpv4.MessageType{dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeInform, dhcpv4.MessageTypeOffer} {
		p := smallPool()
		mods := []dhcpv4.Modifier{}
		if kind == dhcpv4.MessageTypeRequest {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.9.4.99"))))
		}
		m := message(t, kind, macA, mods...)
		reply, e := replyForMessage(handlerConfig(), p, m)
		if e != nil {
			t.Fatal(e)
		}
		if kind == dhcpv4.MessageTypeInform {
			if reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck || reply.IPAddressLeaseTime(0) != 0 {
				t.Fatal("bad INFORM ACK")
			}
		} else if reply != nil {
			t.Fatal("unwanted reply")
		}
		if _, e = p.Allocate(macB); e != nil {
			t.Fatal("non-allocation message used address")
		}
	}
}
func TestDeclineQuarantinesOnlyOwnedAddress(t *testing.T) {
	cfg := handlerConfig()
	p := smallPool()
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	ip, _ := p.Offer(macA)
	m := message(t, dhcpv4.MessageTypeDecline, macA, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(cfg.Gateway))), dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(ip)))
	if _, e := replyForMessage(cfg, p, m); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Allocate(macB); e == nil {
		t.Fatal("declined address immediately reused")
	}
	now = now.Add(declineDuration + time.Second)
	if _, e := p.Allocate(macB); e != nil {
		t.Fatal(e)
	}
}
func TestMalformedAndForeignRequestedAddressDoNotAllocate(t *testing.T) {
	cfg := handlerConfig()
	for _, ip := range []string{"10.9.5.2", "10.9.4.1"} {
		p := smallPool()
		m := message(t, dhcpv4.MessageTypeRequest, macA, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP(ip))))
		r, e := replyForMessage(cfg, p, m)
		if e != nil || r == nil || r.MessageType() != dhcpv4.MessageTypeNak {
			t.Fatalf("invalid address %v %v", r, e)
		}
		if _, e = p.Allocate(macB); e != nil {
			t.Fatal(e)
		}
	}
}
