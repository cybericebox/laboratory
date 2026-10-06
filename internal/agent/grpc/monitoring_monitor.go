package grpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	ctrl "sigs.k8s.io/controller-runtime"
)

// MonitoringConfig bounds the shared monitoring journal and poller. Zero fields
// take the defaults.
type MonitoringConfig struct {
	// JournalSize is the most updates the journal keeps (10000).
	JournalSize int
	// JournalAge is how long an update stays in the journal (15m).
	JournalAge time.Duration
	// PollInterval is how often the one shared poller observes the platform (1s).
	// It is the finest resolution of any subscriber's stream.
	PollInterval time.Duration
	// SubscriberBuffer is how many journal updates a subscriber may lag behind
	// before it is dropped (256).
	SubscriberBuffer int
	// MaxStreamsPerTenant is how many Monitoring streams one tenant may hold open at once (0 = unlimited): each subscribe runs a full
	// collect of the cluster and keeps a buffer, so a tenant must not be able to open them without end.
	MaxStreamsPerTenant int
}

const (
	defaultJournalSize      = 10000
	defaultJournalAge       = 15 * time.Minute
	defaultPollInterval     = time.Second
	defaultSubscriberBuffer = 256
)

func (c MonitoringConfig) withDefaults() MonitoringConfig {
	if c.JournalSize <= 0 {
		c.JournalSize = defaultJournalSize
	}
	if c.JournalAge <= 0 {
		c.JournalAge = defaultJournalAge
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.SubscriberBuffer <= 0 {
		c.SubscriberBuffer = defaultSubscriberBuffer
	}
	return c
}

// journalEntry is one unfiltered update of the platform state with the labels
// needed to decide, per subscriber, whether it concerns them.
type journalEntry struct {
	seq    int64
	at     time.Time
	update *protobuf.MonitoringUpdate
	// labels of the records in the update, and of the deleted ones (taken from the
	// state before the deletion), keyed like recordKey.
	labels map[string]map[string]string
	// labLabels of the labs, for the touches of the traffic reports.
	labLabels map[string]map[string]string
}

// subscriber is one Monitoring stream attached to the monitor.
type subscriber struct {
	ch      chan *journalEntry
	dropped chan struct{}
	once    sync.Once
	reason  error
}

func (s *subscriber) drop(reason error) {
	s.once.Do(func() {
		s.reason = reason
		close(s.dropped)
	})
}

func (s *subscriber) dropErr() error { return s.reason }

// monitor is the one poller and journal behind all Monitoring streams. It
// observes the platform on a ticker while anybody listens, turns each change
// into a journal entry with the next sequence, and hands the entry to every
// subscriber's bounded queue without ever waiting for one.
type monitor struct {
	h     *Handler
	cfg   MonitoringConfig
	epoch string

	pollMu sync.Mutex // one poll at a time
	// cache serves the polls from informers; it exists while somebody is subscribed (guarded by pollMu).
	cache *monCache

	mu    sync.Mutex
	state *monState
	// seq is the sequence of the latest state; the first observation is sequence 1.
	seq     int64
	journal []*journalEntry
	subs    map[*subscriber]struct{}
	running bool
	now     func() time.Time
}

func newEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newMonitor(h *Handler, cfg MonitoringConfig) *monitor {
	return &monitor{h: h, cfg: cfg.withDefaults(), epoch: newEpoch(), seq: 1, subs: map[*subscriber]struct{}{}, now: time.Now}
}

// SetMonitoringConfig sets the journal and poller bounds. Call it before the
// first Monitoring stream; later calls are ignored.
func (h *Handler) SetMonitoringConfig(cfg MonitoringConfig) {
	h.monMaxStreams = cfg.MaxStreamsPerTenant
	h.monOnce.Do(func() { h.mon = newMonitor(h, cfg) })
}

// openStream counts a Monitoring stream of the tenant; false when it holds too many already. closeStream gives the place back.
func (h *Handler) openStream(tenant string) bool {
	h.monMu.Lock()
	defer h.monMu.Unlock()
	if h.monMaxStreams > 0 && h.monStreams[tenant] >= h.monMaxStreams {
		return false
	}
	if h.monStreams == nil {
		h.monStreams = map[string]int{}
	}
	h.monStreams[tenant]++
	return true
}

func (h *Handler) closeStream(tenant string) {
	h.monMu.Lock()
	defer h.monMu.Unlock()
	if h.monStreams[tenant]--; h.monStreams[tenant] <= 0 {
		delete(h.monStreams, tenant)
	}
}

// monitor returns the handler's monitor, created with the defaults when none was configured.
func (h *Handler) monitor() *monitor {
	h.monOnce.Do(func() { h.mon = newMonitor(h, MonitoringConfig{}) })
	return h.mon
}

// startPlan is what a new subscriber begins with: either a snapshot of the
// state, or the journal entries it missed.
type startPlan struct {
	snapshot *monState
	replay   []*journalEntry
	sequence int64
}

// subscribe attaches a subscriber. A request that names this agent's epoch and a
// sequence the journal still covers gets the missed entries; any other gets a
// snapshot. The plan and the registration happen under one lock, so nothing is
// lost between them.
func (m *monitor) subscribe(ctx context.Context, req interface {
	GetResumeAfterSequence() int64
	GetAgentEpoch() string
}) (*subscriber, *startPlan, error) {
	if err := m.poll(ctx); err != nil {
		m.stopCache() // nobody is subscribed: do not leave the informers running
		return nil, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trimLocked()
	sub := &subscriber{ch: make(chan *journalEntry, m.cfg.SubscriberBuffer), dropped: make(chan struct{})}
	plan := &startPlan{sequence: m.seq}
	if after := req.GetResumeAfterSequence(); after > 0 && req.GetAgentEpoch() == m.epoch && m.coversLocked(after) {
		for _, e := range m.journal {
			if e.seq > after {
				plan.replay = append(plan.replay, e)
			}
		}
		plan.sequence = after
	} else {
		plan.snapshot = m.state
	}
	m.subs[sub] = struct{}{}
	if !m.running {
		m.running = true
		go m.run()
	}
	return sub, plan, nil
}

// coversLocked reports whether every entry after seq is still in the journal.
func (m *monitor) coversLocked(after int64) bool {
	if after > m.seq {
		return false
	}
	if after == m.seq {
		return true
	}
	return len(m.journal) > 0 && m.journal[0].seq <= after+1
}

func (m *monitor) unsubscribe(s *subscriber) {
	m.mu.Lock()
	delete(m.subs, s)
	m.mu.Unlock()
}

// run polls while there are subscribers; it stops when the last one is gone.
// The journal and state stay, so the next poll still produces a continuous delta.
func (m *monitor) run() {
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		if len(m.subs) == 0 {
			m.running = false
			m.mu.Unlock()
			m.stopCache()
			return
		}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := m.poll(ctx); err != nil {
			ctrl.Log.WithName("monitoring").Error(err, "observe the platform")
		}
		cancel()
	}
}

// stopCache stops the informers when nobody is subscribed any more; the next subscriber starts them again.
func (m *monitor) stopCache() {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	m.mu.Lock()
	idle := len(m.subs) == 0
	m.mu.Unlock()
	if idle && m.cache != nil {
		m.cache.stop()
		m.cache = nil
	}
}

// ensureCache starts the informers if they are not running (pollMu held).
func (m *monitor) ensureCache(ctx context.Context) (*monCache, error) {
	if m.cache == nil {
		c, err := newMonCache(ctx, m.h)
		if err != nil {
			return nil, err
		}
		m.cache = c
	}
	return m.cache, nil
}

// poll observes the platform once; a change becomes a journal entry offered to every subscriber. A poll that cannot observe
// (the caches did not sync) returns the error and changes nothing: no update, and above all no deletion, for that cycle.
func (m *monitor) poll(ctx context.Context) error {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	c, err := m.ensureCache(ctx)
	if err != nil {
		return err
	}
	next, err := m.h.observe(ctx, c)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.state
	m.state = next
	if prev == nil {
		return nil
	}
	delta, changed := monitoringDelta(prev.update, next.update)
	if !changed {
		return nil
	}
	m.seq++
	entry := &journalEntry{seq: m.seq, at: m.now(), update: delta, labels: map[string]map[string]string{}, labLabels: map[string]map[string]string{}}
	for key := range monitoringRecordIndex(delta) {
		entry.labels[key] = next.labels[key]
	}
	for _, k := range delta.DeletedKeys {
		key := monitoringDeletedKeyString(k)
		entry.labels[key] = prev.labels[key]
	}
	// The touches of a traffic report name labs: keep those labs' labels with the
	// entry (a lab deleted since still has the labels it had).
	for _, r := range delta.Traffic {
		for _, t := range r.GetLedger() {
			key := labLabelKey(r.GetLabGroupName(), t.GetLabName())
			if _, done := entry.labLabels[key]; done {
				continue
			}
			if l, ok := next.labLabels[key]; ok {
				entry.labLabels[key] = l
			} else {
				entry.labLabels[key] = prev.labLabels[key]
			}
		}
	}
	m.journal = append(m.journal, entry)
	m.trimLocked()
	for s := range m.subs {
		select {
		case s.ch <- entry:
		default:
			s.drop(status.Errorf(codes.ResourceExhausted,
				"monitoring subscriber too slow: its buffer of %d updates overflowed; reconnect with resume_after_sequence and agent_epoch (a snapshot follows if the journal no longer covers it)", m.cfg.SubscriberBuffer))
			delete(m.subs, s)
		}
	}
	return nil
}

// trimLocked drops journal entries over the size or age bound.
func (m *monitor) trimLocked() {
	if over := len(m.journal) - m.cfg.JournalSize; over > 0 {
		m.journal = append([]*journalEntry(nil), m.journal[over:]...)
	}
	cut := m.now().Add(-m.cfg.JournalAge)
	i := 0
	for i < len(m.journal) && m.journal[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		m.journal = append([]*journalEntry(nil), m.journal[i:]...)
	}
}

// journalLen is for tests.
func (m *monitor) journalLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.journal)
}
