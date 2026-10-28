package nodeagent

import (
	"bytes"
	"testing"

	"github.com/cybericebox/laboratory/internal/nodeagent/ofclient"
)

func outputs(actions []byte, no uint32) int {
	return bytes.Count(actions, ofclient.BuildActionsOutput(no))
}

func TestPlanT6WithoutLimitsKeepsTheGeneveOutputsInT6(t *testing.T) {
	plan := planT6(100, []uint32{3, 4}, []uint32{0x0a000001, 0x0a000002}, 9, false)
	if len(plan) != 2 {
		t.Fatalf("two flows as before: %d", len(plan))
	}
	local := plan[0]
	if local.table != 6 || local.priority != 110 || local.checked || local.ins.Meter != 0 || local.ins.GotoTable != 0 {
		t.Fatalf("local-origin flood: %+v", local)
	}
	if outputs(local.ins.Actions, 3) != 1 || outputs(local.ins.Actions, 4) != 1 || outputs(local.ins.Actions, 9) != 2 {
		t.Fatalf("local ports once each and the tunnel once per VTEP: %x", local.ins.Actions)
	}
	remote := plan[1]
	if remote.table != 6 || remote.priority != 100 || outputs(remote.ins.Actions, 9) != 0 {
		t.Fatalf("remote-origin flood must not re-flood to Geneve: %+v", remote)
	}
}

func TestPlanT6LimitedSendsOnlyWhatLeavesTheNodeThroughTheMeters(t *testing.T) {
	plan := planT6(100, []uint32{3}, []uint32{0x0a000001, 0x0a000002}, 9, true)
	if len(plan) != 4 {
		t.Fatalf("node-meter table, lab-meter table, t6 local, t6 remote: %d", len(plan))
	}
	node, lab, local, remote := plan[0], plan[1], plan[2], plan[3]
	if node.table != geneveTable2 || node.ins.Meter != nodeMeterID || node.ins.GotoTable != 0 || outputs(node.ins.Actions, 9) != 2 || !node.checked {
		t.Fatalf("t8: %+v", node)
	}
	if lab.table != geneveTable1 || lab.ins.Meter != labMeterID(100) || lab.ins.GotoTable != geneveTable2 || len(lab.ins.Actions) != 0 {
		t.Fatalf("t7: %+v", lab)
	}
	if local.table != 6 || local.ins.GotoTable != geneveTable1 || outputs(local.ins.Actions, 3) != 1 || outputs(local.ins.Actions, 9) != 0 || local.ins.Meter != 0 {
		t.Fatalf("t6 delivers locally without a meter and hands on: %+v", local)
	}
	if remote.table != 6 || remote.ins.GotoTable != 0 || remote.ins.Meter != 0 {
		t.Fatalf("remote origin stays local: %+v", remote)
	}
	// the tables behind the t6 hand-off exist before t6 points at them
	if !(plan[0].table == geneveTable2 && plan[1].table == geneveTable1 && plan[2].table == 6) {
		t.Fatal("the flows must be added back to front")
	}
}

func TestLabMeterIDsFitTheKernelDatapath(t *testing.T) {
	if labMeterID(64999) >= 200000 || labMeterID(0) <= nodeMeterID {
		t.Fatalf("meter ids: %d %d", labMeterID(0), labMeterID(64999))
	}
}
