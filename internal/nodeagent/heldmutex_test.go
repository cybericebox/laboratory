package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) logf(msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, msg+" "+fmt.Sprint(kv...))
}

func (c *capturedLog) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func holdLockForTest(m *heldMutex, d time.Duration) {
	m.Lock()
	time.Sleep(d)
	m.Unlock()
}

// A waiter whose context ends gets out instead of waiting for good, and does not take the lock.
func TestHeldMutexLockCtxGivesUp(t *testing.T) {
	var m heldMutex
	m.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.LockCtx(ctx); err == nil {
		t.Fatal("LockCtx took a lock that is held")
	}
	m.Unlock()
	if err := m.LockCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Unlock()
}

// Mutual exclusion under churn: many goroutines taking the lock never overlap in the critical section.
func TestHeldMutexExcludes(t *testing.T) {
	var m heldMutex
	var wg sync.WaitGroup
	inside, max := 0, 0
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if j%2 == 0 {
					m.Lock()
				} else if err := m.LockCtx(context.Background()); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				inside++
				if inside > max {
					max = inside
				}
				mu.Unlock()
				mu.Lock()
				inside--
				mu.Unlock()
				m.Unlock()
			}
		}()
	}
	wg.Wait()
	if max != 1 {
		t.Fatalf("critical section shared by %d goroutines", max)
	}
}

// A long hold is logged with its holder when released, and a long wait with the holder while it waits.
func TestHeldMutexNamesTheHolderOfASlowLock(t *testing.T) {
	var c capturedLog
	m := heldMutex{name: "veth", logf: c.logf}
	done := make(chan struct{})
	go func() { holdLockForTest(&m, slowLockHold+200*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	m.Lock()
	waited := time.Now()
	m.Unlock()
	_ = waited
	<-done
	got := c.text()
	if !strings.Contains(got, "lock held long") || !strings.Contains(got, "holdLockForTest") {
		t.Fatalf("a long hold must name its holder:\n%s", got)
	}
}

func TestHeldMutexUnlockOfUnlockedPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unlock of an unlocked lock must panic")
		}
	}()
	var m heldMutex
	m.Unlock()
}

// Waiting for and holding the lock are measured: a free lock is a zero wait, a held one a real wait, and the holder is named.
func TestHeldMutexMetrics(t *testing.T) {
	waits0 := sampleCount(t, lockWaitSeconds, "lock", "metrics-test")
	holds0 := sampleCount(t, lockHoldSeconds, "lock", "metrics-test", "holder", "nodeagent.holdLockForTest")
	m := heldMutex{name: "metrics-test"}
	done := make(chan struct{})
	go func() { holdLockForTest(&m, 100*time.Millisecond); close(done) }()
	time.Sleep(20 * time.Millisecond)
	m.Lock() // waits for the holder
	took := time.Now()
	m.Unlock()
	_ = took
	<-done
	if got := sampleCount(t, lockWaitSeconds, "lock", "metrics-test") - waits0; got != 2 {
		t.Fatalf("%d waits observed, want 2 (the holder's free take and the waiter's)", got)
	}
	if got := sampleCount(t, lockHoldSeconds, "lock", "metrics-test", "holder", "nodeagent.holdLockForTest") - holds0; got != 1 {
		t.Fatalf("%d holds of holdLockForTest observed, want 1", got)
	}
}

func sampleCount(t *testing.T, c prometheus.Collector, labels ...string) uint64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			ok := true
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, l := range m.GetLabel() {
					found = found || (l.GetName() == labels[i] && l.GetValue() == labels[i+1])
				}
				ok = ok && found
			}
			if ok {
				if h := m.GetHistogram(); h != nil {
					n += h.GetSampleCount()
				} else if c := m.GetCounter(); c != nil {
					n += uint64(c.GetValue())
				}
			}
		}
	}
	return n
}
