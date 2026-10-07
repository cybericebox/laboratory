package demux

import (
	"testing"
	"time"
)

func TestExpiredIndexReusePreservesNewSession(t *testing.T) {
	c, now := clockTrack(DefaultLimits())
	session(t, c, 10, 20, sock("127.0.0.1", 1), sock("127.0.0.1", 51820))
	*now = now.Add(conntrackTTL + time.Second)
	session(t, c, 10, 30, sock("127.0.0.1", 2), sock("127.0.0.1", 51821))
	c.Cleanup()
	if _, ok := c.Expected(10); !ok {
		t.Fatal("new session deleted by old mirror")
	}
}
func TestSessionActivityKeepsBothDirectionsLive(t *testing.T) {
	c, now := clockTrack(DefaultLimits())
	client, backend := sock("127.0.0.1", 1), sock("127.0.0.1", 51820)
	session(t, c, 10, 20, client, backend)
	*now = now.Add(2 * time.Minute)
	if _, ok := c.LookupForward(20, client); !ok {
		t.Fatal("forward")
	}
	*now = now.Add(2 * time.Minute)
	c.Cleanup()
	if _, ok := c.Expected(10); !ok {
		t.Fatal("active mirror expired")
	}
}
func TestExpiredLookupCannotResurrectSession(t *testing.T) {
	c, now := clockTrack(DefaultLimits())
	client := sock("127.0.0.1", 1)
	session(t, c, 10, 20, client, sock("127.0.0.1", 51820))
	*now = now.Add(conntrackTTL + time.Second)
	if _, ok := c.LookupForward(20, client); ok {
		t.Fatal("expired session resurrected")
	}
}
func TestStopAndCloseReleaseIdleReaders(t *testing.T) {
	for _, closeOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "close"}[closeOnly], func(t *testing.T) {
			d, e := New("127.0.0.1:0", NewTable(), NewConnTrack())
			if e != nil {
				t.Fatal(e)
			}
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() { d.Run(stop); close(done) }()
			time.Sleep(20 * time.Millisecond)
			if closeOnly {
				d.Close()
			} else {
				close(stop)
			}
			defer func() {
				if closeOnly {
					close(stop)
				}
				d.Close()
				<-done
			}()
			select {
			case <-done:
			case <-time.After(100 * time.Millisecond):
				t.Error("idle reader did not stop")
			}
		})
	}
}

func TestGroupRetirementAndZeroIndexDoNotTouchOtherSessions(t *testing.T) {
	c, now := clockTrack(DefaultLimits())
	a, b := sock("127.0.0.1", 1), sock("127.0.0.1", 51820)
	if !c.AddPartial(0, a, b, "keep") || !c.Complete(2, 0, a, b) {
		t.Fatal("zero-index session")
	}
	if !c.AddPartial(10, a, b, "retire") {
		t.Fatal("partial")
	}
	*now = now.Add(20 * time.Second)
	c.Cleanup()
	if _, ok := c.Expected(0); !ok {
		t.Fatal("partial expiry deleted index zero")
	}
	c.RemoveGroup("retire")
	if c.Len() != 2 {
		t.Fatal("unrelated group lost")
	}
	c.RemoveGroup("keep")
	if c.Len() != 0 {
		t.Fatal("group sessions retained")
	}
}
func TestMalformedHeadersNeverUseSession(t *testing.T) {
	r := newRig(t, DefaultLimits())
	r.handshake(t, 1, 2)
	packet := typed(4, 2, 0, 64)
	packet[1] = 1
	r.toDemux(r.client, packet)
	if recv(r.backend, 100*time.Millisecond) != nil {
		t.Fatal("reserved header forwarded")
	}
	packet = typed(4, 2, 0, 33)
	r.toDemux(r.client, packet)
	if recv(r.backend, 100*time.Millisecond) != nil {
		t.Fatal("invalid transport length forwarded")
	}
}
