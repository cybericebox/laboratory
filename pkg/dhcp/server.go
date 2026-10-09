//go:build linux

package dhcp

import (
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

type serverRunner interface {
	Serve() error
	Close() error
}
type StateChange struct {
	Name    string
	Running bool
	Err     error
}
type serverEntry struct {
	handlerMu  sync.Mutex
	handlers   sync.WaitGroup
	stopped    bool
	mu         sync.Mutex
	cfg        Config
	pool       *ipPool
	runner     serverRunner
	done       chan struct{}
	generation uint64
	running    bool
}
type Manager struct {
	op         sync.Mutex
	mu         sync.Mutex
	servers    map[string]*serverEntry
	dormant    map[string]*serverEntry
	generation uint64
	changes    chan StateChange
	closed     bool
	newServer  func(Config, server4.Handler) (serverRunner, error)
}

func NewManager() *Manager {
	return &Manager{
		servers:   map[string]*serverEntry{},
		dormant:   map[string]*serverEntry{},
		changes:   make(chan StateChange, 1),
		newServer: nativeServer,
	}
}
func normalizeConfig(cfg Config) (Config, error) {
	ip, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil || ip.To4() == nil {
		return cfg, fmt.Errorf("invalid IPv4 DHCP subnet")
	}
	ones, bits := subnet.Mask.Size()
	if ones != 24 || bits != 32 {
		return cfg, fmt.Errorf("DHCP requires /24")
	}
	cfg.Subnet = subnet.String()
	if len(cfg.Iface) == 0 || len(cfg.Iface) > 15 || strings.ContainsAny(cfg.Iface, " \t\n/:\x00") {
		return cfg, fmt.Errorf("invalid DHCP interface")
	}
	gw := net.ParseIP(cfg.Gateway).To4()
	if gw == nil || !subnet.Contains(gw) || gw[3] == 0 || gw[3] == 255 {
		return cfg, fmt.Errorf("invalid gateway")
	}
	cfg.Gateway = gw.String()
	if cfg.BindIP == "" {
		cfg.BindIP = cfg.Gateway
	}
	bind := net.ParseIP(cfg.BindIP).To4()
	if bind == nil || !subnet.Contains(bind) || bind[3] == 0 || bind[3] == 255 {
		return cfg, fmt.Errorf("invalid server identifier")
	}
	cfg.BindIP = bind.String()
	if cfg.DNS != "" {
		dns := net.ParseIP(cfg.DNS).To4()
		if dns == nil {
			return cfg, fmt.Errorf("invalid DNS")
		}
		cfg.DNS = dns.String()
	}
	if err := ValidateRanges(cfg.Ranges); err != nil {
		return cfg, err
	}
	cfg.Ranges = append([]Range(nil), cfg.Ranges...)
	sort.Slice(cfg.Ranges, func(i, j int) bool { return cfg.Ranges[i].Start < cfg.Ranges[j].Start })
	return cfg, nil
}
func sameBinding(a, b Config) bool {
	return a.Iface == b.Iface && a.Subnet == b.Subnet && a.Gateway == b.Gateway && a.BindIP == b.BindIP
}

// notify is level-triggered. Overflow coalesces to Name="" (resync all names),
// so a slow controller always queries current health, never stale queued state.
func (m *Manager) notify(change StateChange) {
	select {
	case m.changes <- change:
		return
	default:
	}
	select {
	case <-m.changes:
	default:
	}
	select {
	case m.changes <- StateChange{}:
	default:
	}
}
func (m *Manager) Changes() <-chan StateChange { return m.changes }
func (m *Manager) Healthy(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.servers[name]
	return e != nil && e.running
}
func (m *Manager) Start(name string, cfg Config) error {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("empty DHCP name")
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("DHCP manager closed")
	}
	old := m.servers[name]
	if old != nil && old.running && sameBinding(old.cfg, cfg) {
		old.mu.Lock()
		if !reflect.DeepEqual(old.cfg, cfg) {
			err = old.pool.UpdateRanges(cfg.Ranges)
			if err == nil {
				old.cfg = cfg
			}
		}
		old.mu.Unlock()
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	if old != nil {
		m.stop(name)
	}
	m.mu.Lock()
	poolEntry := m.dormant[name]
	m.mu.Unlock()
	var pool *ipPool
	if poolEntry != nil && sameBinding(poolEntry.cfg, cfg) {
		pool = poolEntry.pool
		_ = pool.UpdateRanges(cfg.Ranges)
	} else {
		_, subnet, _ := net.ParseCIDR(cfg.Subnet)
		pool = newIPPool(subnet, net.ParseIP(cfg.Gateway), cfg.Ranges)
	}
	e := &serverEntry{cfg: cfg, pool: pool, done: make(chan struct{})}
	runner, err := m.newServer(cfg, e.handle)
	if err != nil {
		m.mu.Lock()
		m.notify(StateChange{Name: name, Err: err})
		m.mu.Unlock()
		return fmt.Errorf("new DHCP server on %s: %w", cfg.Iface, err)
	}
	e.runner = runner
	e.running = true
	m.mu.Lock()
	m.generation++
	e.generation = m.generation
	m.servers[name] = e
	delete(m.dormant, name)
	m.notify(StateChange{Name: name, Running: true})
	m.mu.Unlock()
	go func() {
		err := runner.Serve()
		m.mu.Lock()
		if current := m.servers[name]; current == e && current.generation == e.generation && current.running {
			current.running = false
			if err == nil {
				err = fmt.Errorf("DHCP Serve exited")
			}
			m.notify(StateChange{Name: name, Err: err})
		}
		m.mu.Unlock()
		close(e.done)
	}()
	return nil
}

// stop runs with the operation lock held; it releases the state lock while
// waiting so the exiting Serve goroutine can complete synchronously.
func (m *Manager) stop(name string) {
	m.mu.Lock()
	e := m.servers[name]
	if e == nil {
		m.mu.Unlock()
		return
	}
	delete(m.servers, name)
	e.running = false
	m.dormant[name] = e
	m.notify(StateChange{Name: name})
	m.mu.Unlock()
	e.handlerMu.Lock()
	e.stopped = true
	e.handlerMu.Unlock()
	_ = e.runner.Close()
	<-e.done
	e.handlers.Wait()
}
func (m *Manager) Stop(name string) { m.op.Lock(); defer m.op.Unlock(); m.stop(name) }
func (m *Manager) Drop(name string) {
	m.op.Lock()
	defer m.op.Unlock()
	m.stop(name)
	m.mu.Lock()
	delete(m.dormant, name)
	m.mu.Unlock()
}
func (m *Manager) Close() {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	m.closed = true
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.stop(name)
	}
	m.mu.Lock()
	m.dormant = map[string]*serverEntry{}
	m.mu.Unlock()
}
