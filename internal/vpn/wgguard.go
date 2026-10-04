package vpn

import (
	"fmt"
	"strconv"
)

// guardFilter is the part of go-iptables the WireGuard guard uses (a fake stands in for it in the tests).
type guardFilter interface {
	Exists(table, chain string, rulespec ...string) (bool, error)
	Insert(table, chain string, pos int, rulespec ...string) error
	Delete(table, chain string, rulespec ...string) error
}

// The WireGuard listener of the VPN pod binds every address of the pod, so a packet to its UDP port is accepted from whatever
// interface it arrives on, the lab side included. The guard makes the port reachable from one side only: the uplink (eth0),
// where the wg-demux of the proxy forwards the clients' packets. A packet to the port from any other interface (the lab
// interfaces lab<N>, the tunnel interface, the loopback) is dropped. It is an INPUT rule on the interface, whatever the source
// or destination address of the packet is: a lab device cannot choose its way around it by the address it sends from or to.

// wgGuardRules are the INPUT rules for the WireGuard port, in the order they stand: the accept on the uplink first, the drop of
// everything else after it.
func wgGuardRules(uplink string, port int) (accept, drop []string) {
	p := strconv.Itoa(port)
	return []string{"-i", uplink, "-p", "udp", "--dport", p, "-j", "ACCEPT"},
		[]string{"-p", "udp", "--dport", p, "-j", "DROP"}
}

// installWGGuard puts the two rules at the top of INPUT, once. The drop goes in before the accept, so that at no moment is the
// port open to every interface; running again (a container restart in a pod whose network namespace outlives the process) adds
// nothing.
func installWGGuard(ipt guardFilter, uplink string, port int) error {
	accept, drop := wgGuardRules(uplink, port)
	for _, rule := range [][]string{drop, accept} {
		ok, err := ipt.Exists("filter", "INPUT", rule...)
		if err != nil {
			return fmt.Errorf("check the INPUT rule %v: %w", rule, err)
		}
		if ok {
			continue
		}
		if err := ipt.Insert("filter", "INPUT", 1, rule...); err != nil {
			return fmt.Errorf("insert the INPUT rule %v: %w", rule, err)
		}
	}
	return nil
}

// removeWGGuard takes the accept off first and the drop after it: the port is never open to every interface on the way out either.
func removeWGGuard(ipt guardFilter, uplink string, port int) {
	accept, drop := wgGuardRules(uplink, port)
	for _, rule := range [][]string{accept, drop} {
		_ = ipt.Delete("filter", "INPUT", rule...)
	}
}
