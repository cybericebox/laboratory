//go:build linux

package dhcp

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

type fakeServer struct {
	done   chan error
	once   sync.Once
	closed atomic.Bool
}

func (s *fakeServer) Serve() error { return <-s.done }
func (s *fakeServer) Close() error {
	s.once.Do(func() { s.closed.Store(true); close(s.done) })
	return nil
}
func fakeManager(t *testing.T) (*Manager, *[]*fakeServer) {
	t.Helper()
	m := NewManager()
	servers := []*fakeServer{}
	m.newServer = func(_ Config, _ server4.Handler) (serverRunner, error) {
		s := &fakeServer{done: make(chan error, 1)}
		servers = append(servers, s)
		return s, nil
	}
	t.Cleanup(m.Close)
	return m, &servers
}
func TestManagerSameConfigAndReconfigure(t *testing.T) {
	m, servers := fakeManager(t)
	cfg := handlerConfig()
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	if len(*servers) != 1 || !m.Healthy("a") {
		t.Fatal("identical config restarted socket")
	}
	old := m.servers["a"].pool
	ip, err := old.Allocate(macA)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DNS = "8.8.8.8"
	cfg.Ranges = []Range{{2, 3}}
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	if len(*servers) != 1 || m.servers["a"].pool != old {
		t.Fatal("hot update lost lease pool")
	}
	if got, err := old.Commit(macA, ip); err != nil || !got.Equal(ip) {
		t.Fatal("lease lost")
	}
	m.Stop("a")
	if m.Healthy("a") || !(*servers)[0].closed.Load() {
		t.Fatal("Stop not synchronous")
	}
	m.Stop("a")
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	if m.servers["a"].pool != old {
		t.Fatal("disable/enable forgot lease")
	}
	m.Drop("a")
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	if m.servers["a"].pool == old {
		t.Fatal("deleted lab retained pool")
	}
}
func TestManagerServeFailureAndGeneration(t *testing.T) {
	m, servers := fakeManager(t)
	cfg := handlerConfig()
	_ = m.Start("a", cfg)
	(*servers)[0].done <- errors.New("socket failed")
	deadline := time.After(time.Second)
	for m.Healthy("a") {
		select {
		case <-deadline:
			t.Fatal("failed socket healthy")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	if len(*servers) != 2 || !m.Healthy("a") {
		t.Fatal("failed socket not restarted")
	}
	m.Stop("a")
	if !(*servers)[1].closed.Load() {
		t.Fatal("replacement not closed")
	}
}
func TestManagerInvalidConfigDoesNotStopServingSocket(t *testing.T) {
	m, servers := fakeManager(t)
	cfg := handlerConfig()
	_ = m.Start("a", cfg)
	cfg.DNS = "bad"
	if err := m.Start("a", cfg); err == nil {
		t.Fatal("invalid config accepted")
	}
	if !m.Healthy("a") || (*servers)[0].closed.Load() {
		t.Fatal("valid socket stopped before validation")
	}
	_ = net.IPv4zero
}

func TestManagerConcurrentHotUpdateAndLeaseOperations(t *testing.T) {
	m, servers := fakeManager(t)
	cfg := handlerConfig()
	cfg.Ranges = []Range{{2, 4}}
	if err := m.Start("a", cfg); err != nil {
		t.Fatal(err)
	}
	pool := m.servers["a"].pool
	ip, err := pool.Allocate(macA)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			next := cfg
			next.DNS = "8.8.8.8"
			if i%2 == 0 {
				next.DNS = "1.1.1.1"
			}
			if err := m.Start("a", next); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := pool.Commit(macA, ip); err != nil {
				t.Error(err)
			}
			if !pool.Release(macA, ip) {
				t.Error("release lost owner")
			}
			if _, err := pool.Commit(macA, ip); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if len(*servers) != 1 || !m.Healthy("a") {
		t.Fatal("hot update replaced the server")
	}
}

func TestStoppedHandlerCannotReleaseReplacementLease(t *testing.T) {
	m, _ := fakeManager(t)
	cfg := handlerConfig()
	_ = m.Start("a", cfg)
	old := m.servers["a"]
	ip, _ := old.pool.Allocate(macA)
	m.Stop("a")
	_ = m.Start("a", cfg)
	release := message(t, dhcpv4.MessageTypeRelease, macA, dhcpv4.WithClientIP(ip), dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(cfg.Gateway))))
	old.handle(nil, nil, release)
	if _, err := m.servers["a"].pool.Allocate(macB); err == nil {
		t.Fatal("late old RELEASE deleted renewed lease")
	}
}
