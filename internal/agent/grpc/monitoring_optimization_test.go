package grpc

import (
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	labv1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestMonitoringDoesNotAllocateDiscardedSpecJSON(t *testing.T) {
	l := &labv1.Lab{}
	for i := 0; i < 32; i++ {
		l.Spec.Devices = append(l.Spec.Devices, labv1.DeviceTemplate{Name: "web", Type: labv1.DeviceTypeContainer, Image: strings.Repeat("x", 8192)})
	}
	var before, after goruntime.MemStats
	goruntime.ReadMemStats(&before)
	for i := 0; i < 10; i++ {
		p := labMonitoringToProto(l, "g")
		if len(p.SpecJson) != 0 {
			t.Fatal("monitoring leaked Spec")
		}
		goruntime.KeepAlive(p)
	}
	goruntime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / 10; per > 8192 {
		t.Fatalf("discarded Spec projection allocates %d bytes/call", per)
	}
}
func TestMonitoringJournalExpiresWhileIdle(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{PollInterval: 10 * time.Millisecond, JournalAge: 100 * time.Millisecond})
	r.h.monitor().now = time.Now
	r.group("g", "ns", nil)
	sub := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	sub.next(t)
	r.group("g2", "ns2", nil)
	sub.next(t)
	sub.cancel()
	<-sub.done
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.h.monitor().journalLen() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired replay entries retain their objects with no subscribers")
}
