package proxywg

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

// activeWindow is how long a session that sent nothing still counts for the fair share.
const (
	activeWindow  = 5 * time.Second
	shaperRefresh = 2 * time.Second
)

// kernelPath is what the shaper needs of the fast path (*fastpath.FastPath).
type kernelPath interface {
	ByteLimiter() bool
	SetRate(rate float64, burst int) error
	SetBandwidth(sessionKbps, classKbps int64) error
	ActiveSessions(window time.Duration) (int, error)
}

// fastShaper carries the bandwidth limits of the node into the kernel fast path (nil fp: userspace mode, nothing to do). The fair share
// of a session is the class divided by the sessions that were active lately; the kernel counts bytes, so the share is written when it
// changes, not per packet.
type fastShaper struct {
	fp    kernelPath // nil in userspace mode
	base  float64    // the configured packet rate of a session
	burst int
	log   logr.Logger

	mu            sync.Mutex
	limits        bandwidth.Limits
	session, cls  int64 // what is in the kernel now (kbit/s)
	packets       float64
	written, bulk bool
}

func (s *fastShaper) set(l bandwidth.Limits) {
	s.mu.Lock()
	s.limits = l
	s.mu.Unlock()
	s.refresh()
}

func (s *fastShaper) run(ctx context.Context) {
	if s.fp == nil {
		return
	}
	tick := time.NewTicker(shaperRefresh)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.refresh()
		}
	}
}

func (s *fastShaper) refresh() {
	if s.fp == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.limits
	if !s.fp.ByteLimiter() {
		// the fallback: the packet bucket of the session, from the ceiling of a participant
		pps := demux.PacketRateFor(s.base, l)
		if !s.written || pps != s.packets {
			if err := s.fp.SetRate(pps, demux.PacketBurstFor(pps, s.burst)); err != nil {
				s.log.Error(err, "set the packet rate of the fast path")
				return
			}
			s.packets, s.written = pps, true
		}
		return
	}
	if !s.bulk { // the packet bucket keeps its configured rate; the bytes limit
		if err := s.fp.SetRate(s.base, s.burst); err != nil {
			s.log.Error(err, "set the packet rate of the fast path")
			return
		}
		s.bulk = true
	}
	session := int64(0)
	if l.ClassKbps > 0 || l.CapKbps > 0 {
		active := 1
		if l.ClassKbps > 0 {
			n, err := s.fp.ActiveSessions(activeWindow)
			if err != nil {
				s.log.Error(err, "count the active sessions of the fast path")
				return
			}
			active = n
		}
		session = bandwidth.FairShare(l.ClassKbps, l.CapKbps, active)
	}
	if s.written && session == s.session && l.ClassKbps == s.cls {
		return
	}
	if err := s.fp.SetBandwidth(session, l.ClassKbps); err != nil {
		s.log.Error(err, "set the byte limits of the fast path")
		return
	}
	s.session, s.cls, s.written = session, l.ClassKbps, true
}
