package registrygate

import "sync"

// flight runs one function per key at a time: callers of the same key wait for the one that runs and share its error (singleflight, in
// the little of it the gate needs).
type flight struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	done chan struct{}
	err  error
}

// Do runs fn if no call for key is in flight and returns (err, true); otherwise it waits for that call and returns (its err, false).
func (f *flight) Do(key string, fn func() error) (err error, leader bool) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*call{}
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.err, false
	}
	c := &call{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.err = fn()
	return c.err, true
}

// Start is Do for a call nobody waits for: it runs fn in the background unless one for key is already running.
func (f *flight) Start(key string, fn func()) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*call{}
	}
	if _, ok := f.calls[key]; ok {
		f.mu.Unlock()
		return
	}
	c := &call{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	go func() {
		defer func() {
			f.mu.Lock()
			delete(f.calls, key)
			f.mu.Unlock()
			close(c.done)
		}()
		fn()
	}()
}
