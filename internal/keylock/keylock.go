// Package keylock is a set of mutexes addressed by a string key. Two goroutines with the same key exclude each other; different
// keys never wait for one another. An entry lives only while somebody holds or waits for it, so the set does not grow with the keys.
package keylock

import "sync"

// Set is the zero-value-ready set of key mutexes.
type Set struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	mu   sync.Mutex
	refs int
}

// Lock takes the mutex of key, waiting for its holder, and returns the function that releases it. The function may be called once.
func (s *Set) Lock(key string) (unlock func()) {
	s.mu.Lock()
	if s.entries == nil {
		s.entries = map[string]*entry{}
	}
	e := s.entries[key]
	if e == nil {
		e = &entry{}
		s.entries[key] = e
	}
	e.refs++
	s.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		s.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(s.entries, key)
		}
		s.mu.Unlock()
	}
}

// Len is the number of keys held or waited for now (for tests).
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
