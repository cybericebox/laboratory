package keylock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSameKeyExcludes(t *testing.T) {
	var s Set
	var inside, worst atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := s.Lock("ns")
			n := inside.Add(1)
			if n > worst.Load() {
				worst.Store(n)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if worst.Load() != 1 {
		t.Fatalf("%d goroutines held one key at once", worst.Load())
	}
	if s.Len() != 0 {
		t.Fatalf("%d entries left behind", s.Len())
	}
}

func TestDifferentKeysDoNotWait(t *testing.T) {
	var s Set
	unlockA := s.Lock("a")
	done := make(chan struct{})
	go func() {
		s.Lock("b")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a key waited for another key")
	}
	unlockA()
}
