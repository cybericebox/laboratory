//go:build linux

package cnigate

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
)

// A real network namespace that was created and then removed (what the kubelet's DEL sees for a pod whose sandbox is gone):
// DEL succeeds although the delegate fails, while the same delegate failure on a live namespace is still reported.
func TestNetnsDelWithRemovedNetns(t *testing.T) {
	nstest.Require(t)
	marker := fakeDelegate(t)

	nstest.NS(t, "podgone")
	nstest.NS(t, "podlive")
	path := func(n string) string { return "/var/run/netns/" + n }

	if err := cmdDEL(delArgs(path("podlive"))); err == nil {
		t.Error("a delegate failure on a live netns must be reported")
	}

	nstest.Run(t, "", "ip", "netns", "del", "podgone")
	if err := cmdDEL(delArgs(path("podgone"))); err != nil {
		t.Errorf("DEL with a removed netns must succeed: %v", err)
	}
	nstest.Run(t, "", "test", "-e", marker)
}
