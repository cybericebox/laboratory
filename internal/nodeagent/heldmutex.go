package nodeagent

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Waits and holds longer than these are logged with who held the lock and who waited.
const (
	slowLockWait = 5 * time.Second
	slowLockHold = 2 * time.Second
)

// heldMutex is a mutex that says what it is busy with. The veth lock serializes the wiring of every Pod of the node against the
// observers and cleanups; under churn it is held across OVS transactions, API reads and flow barriers, and a CNI hook that waits
// for it waits without a bound and without anybody knowing who holds it. This one logs a wait longer than slowLockWait with the
// function that holds the lock and for how long, logs a hold longer than slowLockHold at release, and LockCtx gives a hook that
// cannot wait any longer (its caller gave up) a way out. Lock and Unlock work like sync.Mutex's.
type heldMutex struct {
	name string
	once sync.Once
	ch   chan struct{}

	mu     sync.Mutex // guards the holder fields
	holder string
	since  time.Time

	// logf is the log of a slow wait or hold; nil means the controller-runtime logger.
	logf func(msg string, kv ...any)
}

func (m *heldMutex) init() { m.once.Do(func() { m.ch = make(chan struct{}, 1) }) }

func (m *heldMutex) log(msg string, kv ...any) {
	if m.logf != nil {
		m.logf(msg, kv...)
		return
	}
	ctrl.Log.WithName("lock").Info(msg, kv...)
}

func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	name := runtime.FuncForPC(pc).Name()
	return name[strings.LastIndex(name, "/")+1:]
}

func (m *heldMutex) acquired(who string) {
	m.mu.Lock()
	m.holder, m.since = who, time.Now()
	m.mu.Unlock()
}

// Lock waits for the lock as long as it takes, logging every slowLockWait that passes.
func (m *heldMutex) Lock() { _ = m.lock(context.Background(), callerName(2)) }

// LockCtx is Lock that gives up when ctx ends; the lock is then not held and the error is ctx's.
func (m *heldMutex) LockCtx(ctx context.Context) error { return m.lock(ctx, callerName(2)) }

func (m *heldMutex) lock(ctx context.Context, who string) error {
	m.init()
	select {
	case m.ch <- struct{}{}:
		m.acquired(who)
		return nil
	default:
	}
	start := time.Now()
	t := time.NewTicker(slowLockWait)
	defer t.Stop()
	for {
		select {
		case m.ch <- struct{}{}:
			m.acquired(who)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.mu.Lock()
			holder, since := m.holder, m.since
			m.mu.Unlock()
			m.log("waiting for a lock", "lock", m.name, "waiter", who, "waited", time.Since(start).Round(time.Millisecond).String(),
				"holder", holder, "heldFor", time.Since(since).Round(time.Millisecond).String())
		}
	}
}

// Unlock releases the lock.
func (m *heldMutex) Unlock() {
	m.mu.Lock()
	holder, held := m.holder, time.Since(m.since)
	m.holder = ""
	m.mu.Unlock()
	if held > slowLockHold {
		m.log("lock held long", "lock", m.name, "holder", holder, "heldFor", held.Round(time.Millisecond).String())
	}
	select {
	case <-m.ch:
	default:
		panic("nodeagent: unlock of an unlocked heldMutex")
	}
}
