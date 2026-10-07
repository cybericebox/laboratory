//go:build linux

package dhcp

import (
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"net"
	"testing"
)

func TestNetnsHelperProcess(*testing.T) { nstest.HelperProcess() }
func exchange(t *testing.T, ns string, cfg Config, kind dhcpv4.MessageType, ip net.IP, broadcast bool) *dhcpv4.DHCPv4 {
	t.Helper()
	mods := []dhcpv4.Modifier{dhcpv4.WithBroadcast(broadcast)}
	if kind == dhcpv4.MessageTypeRequest || kind == dhcpv4.MessageTypeRelease {
		mods = append(mods, dhcpv4.WithClientIP(ip))
	}
	if kind == dhcpv4.MessageTypeRelease {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(cfg.Gateway))))
	}
	m := message(t, kind, macA, mods...)
	reply, err := nstest.DHCP(ns, cfg.Gateway, m)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}
func TestNetnsTwoDHCPServersHotUpdateAndRelease(t *testing.T) {
	nstest.Require(t)
	nstest.NS(t, "a")
	nstest.NS(t, "b")
	nstest.Veth(t, "", "lab1", "10.9.4.1/24", "a", "a0", "10.9.4.99/24")
	nstest.Veth(t, "", "lab2", "10.9.5.1/24", "b", "b0", "10.9.5.99/24")
	t.Cleanup(func() {
		for _, i := range []string{"lab1", "lab2"} {
			_, _ = nstest.Try("", "ip", "link", "del", i)
		}
	})
	mgr := NewManager()
	t.Cleanup(mgr.Close)
	a := handlerConfig()
	b := a
	b.Iface = "lab2"
	b.Subnet = "10.9.5.0/24"
	b.Gateway = "10.9.5.1"
	b.DNS = "8.8.4.4"
	if err := mgr.Start("a", a); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start("b", b); err != nil {
		t.Fatal(err)
	}
	offer := exchange(t, "a", a, dhcpv4.MessageTypeDiscover, nil, true)
	if !offer.YourIPAddr.Equal(net.ParseIP("10.9.4.2")) {
		t.Fatal("wrong offer")
	}
	request := message(t, dhcpv4.MessageTypeRequest, macA, dhcpv4.WithBroadcast(true), dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(a.Gateway))), dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(offer.YourIPAddr)))
	ack, err := nstest.DHCP("a", a.Gateway, request)
	if err != nil || ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("request %v %v", ack, err)
	}
	other := exchange(t, "b", b, dhcpv4.MessageTypeDiscover, nil, true)
	if !other.YourIPAddr.Equal(net.ParseIP("10.9.5.2")) || !other.DNS()[0].Equal(net.ParseIP(b.DNS)) {
		t.Fatal("pool/interface isolation failed")
	}
	a.DNS = "8.8.8.8"
	a.Ranges = []Range{{2, 3}}
	if err := mgr.Start("a", a); err != nil {
		t.Fatal(err)
	}
	renewed := exchange(t, "a", a, dhcpv4.MessageTypeRequest, offer.YourIPAddr, false)
	if renewed.MessageType() != dhcpv4.MessageTypeAck || !renewed.DNS()[0].Equal(net.ParseIP(a.DNS)) {
		t.Fatal("hot DNS/renew failed")
	}
	exchange(t, "a", a, dhcpv4.MessageTypeRelease, offer.YourIPAddr, false)
	mgr.Stop("a")
	if mgr.Healthy("a") {
		t.Fatal("stopped server ready")
	}
	if err := mgr.Start("a", a); err != nil {
		t.Fatal(err)
	}
	m := message(t, dhcpv4.MessageTypeDiscover, macB, dhcpv4.WithBroadcast(true))
	fresh, err := nstest.DHCP("a", a.Gateway, m)
	if err != nil || !fresh.YourIPAddr.Equal(offer.YourIPAddr) {
		t.Fatalf("release/restart %v %v", fresh, err)
	}
	mgr.Drop("a")
	if mgr.Healthy("a") {
		t.Fatal("deleted lab ready")
	}
}
