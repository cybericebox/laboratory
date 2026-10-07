//go:build linux

package nodeagent

import (
	"testing"
	"time"
)

func TestExistingPeerLookupDoesNotPoll(t *testing.T) {
	start := time.Now()
	if peerInRoot("cice-absent", false) {
		t.Fatal("absent peer reported in root namespace")
	}
	if elapsed := time.Since(start); elapsed >= 150*time.Millisecond {
		t.Fatalf("existing peer lookup polled for %s; moved peers need one lookup", elapsed)
	}
	if !peerInRoot("lo", false) {
		t.Fatal("existing link in root namespace was missed")
	}
}

func TestNewPeerLookupKeepsCreationWait(t *testing.T) {
	start := time.Now()
	if peerInRoot("cice-absent", true) {
		t.Fatal("absent peer reported as created")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("new peer creation wait ended early: %s", elapsed)
	}
}
