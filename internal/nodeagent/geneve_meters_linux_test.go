//go:build linux

package nodeagent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/nstest"
)

// The Geneve meters and the t6/t7/t8 pipeline against a real Open vSwitch with the kernel datapath: the meters exist with the right rates,
// the flows chain through them, a rate change is a modify that keeps the flows, and a lab that is no longer split loses its meter.
func TestNetnsGeneveMetersProgramOpenVSwitch(t *testing.T) {
	nstest.Require(t)
	ovs, flows, dir, sock := nativeOwnerOVS(t)
	for _, port := range []string{"p1", "p2"} {
		nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "add-port", "t6-br", port, "--", "set", "interface", port, "type=internal")
	}
	if err := ovs.AddGenevePort("", ""); err != nil {
		t.Fatal(err)
	}
	bridge := "unix:" + filepath.Join(dir, "t6-br.mgmt")
	ofctl := func(args ...string) string {
		return nstest.Run(t, "", append([]string{"ovs-ofctl", "-O", "OpenFlow13", args[0], bridge}, args[1:]...)...)
	}
	meters := func() string { return ofctl("dump-meters") }
	has := func(out, want string) bool { return strings.Contains(out, want) }

	// no limits: the Geneve outputs stay in t6, no meter is made
	if err := flows.RebuildT6Flood(100, []string{"p1"}, []string{"10.99.0.2"}); err != nil {
		t.Fatal(err)
	}
	if has(meters(), "meter=") {
		t.Fatalf("no limits, no meters:\n%s", meters())
	}
	if !has(ofctl("dump-flows", "table=6"), "set_field:0x64->tun_id") && !has(ofctl("dump-flows", "table=6"), "tun_dst") {
		t.Fatalf("without limits t6 still sends to the tunnel:\n%s", ofctl("dump-flows", "table=6"))
	}

	// limits on: the split lab gets its meter, the node its own, and the flows are rebuilt through the tables 7 and 8
	if err := flows.SetGeneveLimits(bandwidth.Limits{GeneveKbps: 240000, LabCapKbps: 100000}); err != nil {
		t.Fatal(err)
	}
	m := meters()
	if !has(m, "meter=1 kbps") || !has(m, "rate=240000") || !has(m, "meter=116 kbps") || !has(m, "rate=100000") {
		t.Fatalf("the node meter (1) at 240000 and the lab meter (116 = 16 + VNI) at 100000:\n%s", m)
	}
	t7, t8, t6 := ofctl("dump-flows", "table=7"), ofctl("dump-flows", "table=8"), ofctl("dump-flows", "table=6")
	if !has(t7, "meter:116") || !has(t7, "goto_table:8") {
		t.Fatalf("t7:\n%s", t7)
	}
	if !has(t8, "meter:1") || !has(t8, "output") {
		t.Fatalf("t8:\n%s", t8)
	}
	if !has(t6, "goto_table:7") {
		t.Fatalf("t6 hands the packet on to t7:\n%s", t6)
	}

	// a second split lab shares the class; a change of rate is a modify, the flows stay
	if err := flows.RebuildT6Flood(101, []string{"p2"}, []string{"10.99.0.2"}); err != nil {
		t.Fatal(err)
	}
	if err := flows.SetGeneveLimits(bandwidth.Limits{GeneveKbps: 120000, LabCapKbps: 100000}); err != nil {
		t.Fatal(err)
	}
	m = meters()
	if !has(m, "meter=1 kbps") || strings.Count(m, "rate=60000") != 2 || !has(m, "rate=120000") {
		t.Fatalf("two labs of a 120000 class: 60000 each, the node 120000:\n%s", m)
	}
	if !has(ofctl("dump-flows", "table=7"), "meter:116") || !has(ofctl("dump-flows", "table=7"), "meter:117") {
		t.Fatalf("the flows of both labs survive a rate change:\n%s", ofctl("dump-flows", "table=7"))
	}

	// a lab that floods to two remote nodes counts twice in the meter: its rate halves
	if err := flows.RebuildT6Flood(101, []string{"p2"}, []string{"10.99.0.2", "10.99.0.3"}); err != nil {
		t.Fatal(err)
	}
	if m = meters(); !has(m, "rate=30000") {
		t.Fatalf("a lab flooded to two nodes: 60000 / 2:\n%s", m)
	}

	// the lab that is no longer split loses its meter and its flows; the other keeps its
	if err := flows.RebuildT6Flood(101, []string{"p2"}, nil); err != nil {
		t.Fatal(err)
	}
	m = meters()
	if has(m, "meter=117 ") {
		t.Fatalf("meter 117 should be gone:\n%s", m)
	}
	if !has(m, "meter=116 kbps") {
		t.Fatalf("meter 116 stays:\n%s", m)
	}
	if err := flows.DelT6Flood(100); err != nil {
		t.Fatal(err)
	}
	if has(meters(), "meter=116 ") || has(ofctl("dump-flows", "table=7"), "meter:116") {
		t.Fatalf("a deleted lab leaves nothing:\n%s\n%s", meters(), ofctl("dump-flows", "table=7"))
	}

	// the limits go off: the split labs are built without meters, and no meter is left
	if err := flows.RebuildT6Flood(100, []string{"p1"}, []string{"10.99.0.2"}); err != nil {
		t.Fatal(err)
	}
	if err := flows.SetGeneveLimits(bandwidth.Limits{}); err != nil {
		t.Fatal(err)
	}
	if has(meters(), "meter=") || has(ofctl("dump-flows", "table=7"), "meter:") || has(ofctl("dump-flows", "table=6"), "goto_table") {
		t.Fatalf("limits off:\n%s\n%s\n%s", meters(), ofctl("dump-flows", "table=7"), ofctl("dump-flows", "table=6"))
	}
}
