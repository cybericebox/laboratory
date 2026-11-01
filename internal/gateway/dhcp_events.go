//go:build linux

package gateway

import (
	"sync"

	"github.com/vishvananda/netlink"

	"github.com/cybericebox/laboratory/pkg/netutil"
)

type appliedNetwork struct {
	Iface, CIDR, Hardware string
	LinkIndex             int
	DHCP                  bool
	DHCPKnown             bool
}

// appliedMap is what the reconciler has put on each lab interface. Several labs are reconciled at once, so it is guarded.
type appliedMap struct {
	mu sync.Mutex
	m  map[string]appliedNetwork
}

func (a *appliedMap) get(name string) (appliedNetwork, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.m[name]
	return v, ok
}

func (a *appliedMap) set(name string, v appliedNetwork) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]appliedNetwork{}
	}
	a.m[name] = v
}

func (a *appliedMap) del(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.m, name)
}

// update changes the entry of name if there is one.
func (a *appliedMap) update(name string, f func(*appliedNetwork)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.m[name]; ok {
		f(&v)
		a.m[name] = v
	}
}

func networkPresent(link netlink.Link, cidr string) bool {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if addr.IPNet != nil && addr.IPNet.String() == firstHostIP(cidr)+"/24" {
			return true
		}
	}
	return false
}
func removeNetworkAddress(state appliedNetwork) { netutil.RemoveFirstHostIP(state.Iface, state.CIDR) }
