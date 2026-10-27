package nodeagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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
	m.Unlock()
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
