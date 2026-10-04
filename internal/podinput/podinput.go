// Package podinput is the INPUT policy of the VPN pod and the internet gateway pod.
//
// Both pods are transparent gateways: what they forward is the business of their FORWARD rules, and nothing on the lab side may
// talk to the pod itself. Every lab interface (lab<N>) and every other interface that is not the uplink gets its packets that
// are addressed to the pod dropped, ICMP included, so the pod does not answer a ping, a port scan or a connection on its own
// addresses. What stays open, and only that:
//
//   - the loopback;
//   - replies to what the pod itself started (conntrack ESTABLISHED, RELATED), and the continuation of a connection that an
//     earlier rule accepted;
//   - everything from the uplink (the pod network: the kubelet, the node, the proxy that delivers WireGuard to its UDP port,
//     which wgguard.go narrows further);
//   - the VPN status page, from the WireGuard interface and to the tunnel address only;
//   - DHCP, on the lab interfaces where the pod runs a DHCP server (AllowDHCP);
//   - ping of the pod's own address on a lab interface, rate-limited, and only on the interface that owns the address (AllowPing):
//     Linux answers for every local address on every interface, so without the -d match a device could ping the pod's address
//     of another lab, its uplink or its tunnel address;
//   - IPv6 neighbour discovery on the lab interfaces (ARP of IPv4 is not netfilter's).
//
// The rules live in a chain of their own, jumped to from the top of INPUT. The chain gets its last rule (the drop) first and the
// jump last, so no moment of the installation drops what should be accepted or accepts what should be dropped.
package podinput

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Filter is the part of go-iptables the policy uses (a fake stands in for it in the tests).
type Filter interface {
	ChainExists(table, chain string) (bool, error)
	ClearChain(table, chain string) error
	ClearAndDeleteChain(table, chain string) error
	Exists(table, chain string, rulespec ...string) (bool, error)
	Insert(table, chain string, pos int, rulespec ...string) error
	Append(table, chain string, rulespec ...string) error
	Delete(table, chain string, rulespec ...string) error
}

// Chain is the chain of the policy.
const Chain = "CICE_INPUT"

// LabIfaces matches every lab interface of a pod (lab<N>).
const LabIfaces = "lab+"

// accept and drop are the targets of the rules.
const (
	accept = "ACCEPT"
	drop   = "DROP"
)

// dhcpPort is the port of a DHCP server.
const dhcpPort = "67"

// Probe is the one service of the pod that a participant may reach: a TCP port on the tunnel address, from the tunnel interface.
type Probe struct {
	Iface string
	Addr  string
	Port  int
}

// Policy is what the pod needs.
type Policy struct {
	// Uplink is the pod network interface (eth0): everything from it is accepted.
	Uplink string
	// Probe is the VPN status page; nil for a pod that has none (the gateway).
	Probe *Probe
}

// Rules are the accepts of the chain, in the order they stand, for IPv4 or IPv6. The drop of everything else is the last rule of
// the chain and is not in the list.
func Rules(p Policy, ipv6 bool) [][]string {
	rules := [][]string{
		{"-i", "lo", "-j", accept},
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", accept},
		{"-i", p.Uplink, "-j", accept},
	}
	if p.Probe != nil && !ipv6 {
		rules = append(rules, []string{
			"-i", p.Probe.Iface, "-d", p.Probe.Addr, "-p", "tcp", "--dport", strconv.Itoa(p.Probe.Port), "-j", accept,
		})
	}
	if ipv6 {
		for _, t := range []string{"neighbour-solicitation", "neighbour-advertisement"} {
			rules = append(rules, []string{"-i", LabIfaces, "-p", "icmpv6", "--icmpv6-type", t, "-j", accept})
		}
	}
	return rules
}

var dropRule = []string{"-j", drop}

// DHCPRule accepts DHCP requests that arrive on one lab interface.
func DHCPRule(iface string) []string {
	return []string{"-i", iface, "-p", "udp", "--dport", dhcpPort, "-j", accept}
}

// Install builds the chain and jumps to it from INPUT. It is idempotent (a container restart in a pod whose network namespace
// outlives the process finds the rules in place). Run it before any interface of the pod comes up.
func Install(ipt Filter, p Policy, ipv6 bool) error {
	ok, err := ipt.ChainExists("filter", Chain)
	if err != nil {
		return fmt.Errorf("check chain %s: %w", Chain, err)
	}
	if !ok {
		if err := ipt.ClearChain("filter", Chain); err != nil { // creates a missing chain
			return fmt.Errorf("create chain %s: %w", Chain, err)
		}
	}
	// The drop first, then the accepts in front of it (inserted last-to-first so they stand in list order).
	if err := ensure(ipt, Chain, dropRule, false); err != nil {
		return err
	}
	rules := Rules(p, ipv6)
	for i := len(rules) - 1; i >= 0; i-- {
		if err := ensure(ipt, Chain, rules[i], true); err != nil {
			return err
		}
	}
	return ensure(ipt, "INPUT", []string{"-j", Chain}, true)
}

// ensure adds a rule once: at the top when top is set, at the end otherwise.
func ensure(ipt Filter, chain string, rule []string, top bool) error {
	ok, err := ipt.Exists("filter", chain, rule...)
	if err != nil {
		return fmt.Errorf("check the %s rule %v: %w", chain, rule, err)
	}
	if ok {
		return nil
	}
	if top {
		err = ipt.Insert("filter", chain, 1, rule...)
	} else {
		err = ipt.Append("filter", chain, rule...)
	}
	if err != nil {
		return fmt.Errorf("add the %s rule %v: %w", chain, rule, err)
	}
	return nil
}

// AllowDHCP opens DHCP on one lab interface; DenyDHCP closes it again. Both are idempotent. The policy must be installed.
func AllowDHCP(ipt Filter, iface string) error {
	return ensure(ipt, Chain, DHCPRule(iface), true)
}

func DenyDHCP(ipt Filter, iface string) { deleteAll(ipt, DHCPRule(iface)) }

// pingRate is the rate limit of echo requests per lab interface: a ping to the gateway works, a flood does not.
const (
	pingRate  = "10/second"
	pingBurst = "20"
)

// PingRule accepts echo requests that arrive on one lab interface and are addressed to that interface's own address.
func PingRule(iface, addr string) []string {
	return []string{"-i", iface, "-d", addr, "-p", "icmp", "--icmp-type", "echo-request",
		"-m", "limit", "--limit", pingRate, "--limit-burst", pingBurst, "-j", accept}
}

// pingOverLimitRule drops the echo requests over the limit. It must be there: conntrack keeps an echo "connection" by id, so after
// the first answer every further request of the same ping is ESTABLISHED, and without this rule the ESTABLISHED accept would let
// the over-limit ones through.
func pingOverLimitRule(iface, addr string) []string {
	return []string{"-i", iface, "-d", addr, "-p", "icmp", "--icmp-type", "echo-request", "-j", drop}
}

// AllowPing lets a lab ping the pod's address on its own interface (the answer is the pod's own output, which is not filtered);
// DenyPing takes it away with the interface. Both are idempotent. The policy must be installed. The two rules stand before the
// ESTABLISHED accept (they go to the top of the chain), the drop of the over-limit requests first so the accept is never alone.
func AllowPing(ipt Filter, iface, addr string) error {
	if err := ensure(ipt, Chain, pingOverLimitRule(iface, addr), true); err != nil {
		return err
	}
	return ensure(ipt, Chain, PingRule(iface, addr), true)
}

func DenyPing(ipt Filter, iface, addr string) {
	deleteAll(ipt, PingRule(iface, addr))
	deleteAll(ipt, pingOverLimitRule(iface, addr))
}

func deleteAll(ipt Filter, rule []string) {
	for {
		ok, err := ipt.Exists("filter", Chain, rule...)
		if err != nil || !ok {
			return
		}
		if ipt.Delete("filter", Chain, rule...) != nil {
			return
		}
	}
}

// ipv6Sysctls are the switches that turn the IPv6 stack of the pod's network namespace off.
var ipv6Sysctls = []string{"/proc/sys/net/ipv6/conf/all/disable_ipv6", "/proc/sys/net/ipv6/conf/default/disable_ipv6"}

// DisableIPv6 turns IPv6 off in the pod's network namespace. A pod without a working ip6tables cannot filter IPv6, so it must not
// have any: otherwise the lab side would reach it over IPv6 with no INPUT policy at all.
func DisableIPv6() error { return writeSysctls(ipv6Sysctls, "1") }

// ConntrackHelpers is the switch of automatic conntrack helpers (FTP, SIP, ...): a helper makes a RELATED connection out of a
// payload, which the ESTABLISHED,RELATED accept of the FORWARD chain would let through.
const ConntrackHelpers = "/proc/sys/net/netfilter/nf_conntrack_helper"

// NoConntrackHelpers makes sure no conntrack helper is assigned automatically (the default of kernels since 4.7; the file is
// absent when conntrack is not loaded yet, which is fine). It tries to switch the helpers off when they are on.
func NoConntrackHelpers(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if strings.TrimSpace(string(b)) == "0" {
		return nil
	}
	if err := writeSysctls([]string{path}, "0"); err != nil {
		return fmt.Errorf("conntrack helpers are on and cannot be switched off: %w", err)
	}
	return nil
}

func writeSysctls(paths []string, v string) error {
	for _, p := range paths {
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	return nil
}

// Remove takes the policy off: the jump first, then the chain.
func Remove(ipt Filter) {
	for {
		ok, err := ipt.Exists("filter", "INPUT", "-j", Chain)
		if err != nil || !ok {
			break
		}
		if ipt.Delete("filter", "INPUT", "-j", Chain) != nil {
			break
		}
	}
	_ = ipt.ClearAndDeleteChain("filter", Chain)
}
