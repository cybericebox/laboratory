package demux

import (
	"context"
	"encoding/base64"
	"net"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestUnchangedTableDoesNotResolveAgain(t *testing.T) {
	table := NewTable()
	calls, changes := 0, 0
	table.resolve = func(context.Context, string) (*net.UDPAddr, error) {
		calls++
		return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51820}, nil
	}
	table.OnChange = func(string) { changes++ }
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	_ = table.Update("g", pub, "vpn.example:51820")
	_ = table.Update("g", pub, "vpn.example:51820")
	if calls != 1 || changes != 1 {
		t.Fatalf("resolves=%d changes=%d", calls, changes)
	}
	table.Delete("g")
	if changes != 2 {
		t.Fatal("removal not invalidated")
	}
}
func TestDemuxRoutingInputsIgnoreOtherStatus(t *testing.T) {
	old := &lab.LabGroup{}
	next := old.DeepCopy()
	next.Status.Phase = lab.PhaseReady
	if routingInputsChanged(old, next) {
		t.Fatal("phase changed routing")
	}
	next.Status.VPN.Registered = true
	if !routingInputsChanged(old, next) {
		t.Fatal("registration lost")
	}
}

func TestGroupKeyChangeRetiresItsSessions(t *testing.T) {
	table := NewTable()
	ct := NewConnTrack()
	table.OnChange = ct.RemoveGroup
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	_ = table.Update("g", pub, "127.0.0.1:51820")
	a, b := sock("127.0.0.1", 1), sock("127.0.0.1", 51820)
	ct.AddPartial(1, a, b, "g")
	ct.Complete(2, 1, a, b)
	raw := make([]byte, 32)
	raw[0] = 1
	if err := table.Update("g", base64.StdEncoding.EncodeToString(raw), "127.0.0.1:51820"); err != nil {
		t.Fatal(err)
	}
	if ct.Len() != 0 {
		t.Fatal("old key sessions retained")
	}
}

func TestDeleteWaitsForPendingUpdate(t *testing.T) {
	table := NewTable()
	entered, release := make(chan struct{}), make(chan struct{})
	table.resolve = func(context.Context, string) (*net.UDPAddr, error) {
		close(entered)
		<-release
		return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51820}, nil
	}
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	updated := make(chan struct{})
	go func() { defer close(updated); _ = table.Update("g", pub, "vpn.example:51820") }()
	<-entered
	deleted := make(chan struct{})
	go func() { table.Delete("g"); close(deleted) }()
	select {
	case <-deleted:
		close(release)
		<-updated
		t.Fatal("deletion returned before the pending update; entry can be resurrected")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-updated
	<-deleted
	table.mu.RLock()
	defer table.mu.RUnlock()
	if len(table.entries) != 0 {
		t.Fatal("entry survived deletion")
	}
}

func TestResolverCancellationJoins(t *testing.T) {
	table := NewTable()
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	_ = table.Update("g", pub, "127.0.0.1:51820")
	entered := make(chan struct{})
	table.resolve = func(ctx context.Context, _ string) (*net.UDPAddr, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { table.RunResolverContext(ctx, time.Hour); close(done) }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resolver did not stop")
	}
}
