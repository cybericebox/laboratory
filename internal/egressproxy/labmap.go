package egressproxy

import (
	"net/netip"
	"sync"
)

// LabMap says which lab a source address belongs to: the lab of the /24 it sits in (every lab of a group has its own subnet of the internet
// network). The gateway reconciler keeps it current.
type LabMap struct {
	mu    sync.RWMutex
	labs  map[netip.Prefix]string
	order []netip.Prefix
}

func NewLabMap() *LabMap { return &LabMap{labs: map[netip.Prefix]string{}} }

// Set puts a lab's subnet in; Delete takes it out.
func (m *LabMap) Set(cidr, lab string) error {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.labs[p]; !ok {
		m.order = append(m.order, p)
	}
	m.labs[p] = lab
	return nil
}

func (m *LabMap) Delete(cidr string) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.labs, p)
	for i, q := range m.order {
		if q == p {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// Lookup returns the lab of an address.
func (m *LabMap) Lookup(a netip.Addr) (string, bool) {
	a = a.Unmap()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.order {
		if p.Contains(a) {
			return m.labs[p], true
		}
	}
	return "", false
}
