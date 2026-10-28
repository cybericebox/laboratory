package nodeagent

import "github.com/cybericebox/laboratory/internal/nodeagent/ofclient"

// t6Flow is one flow of the flood pipeline of a VNI.
type t6Flow struct {
	what     string
	table    uint8
	priority uint16
	match    []byte
	ins      ofclient.Instructions
	checked  bool // confirmed with a barrier (the flow names a meter or a next table)
}

// planT6 lists the flows of a VNI's flood: the local outputs, and the Geneve outputs either in t6 itself or, limited, behind
// the lab meter (t7) and the node meter (t8).
func planT6(vni uint, localNos, remoteVTEPs []uint32, geneveNo uint32, limited bool) []t6Flow {
	var localActions []byte
	for _, no := range localNos {
		localActions = append(localActions, ofclient.BuildActionsOutput(no)...)
	}
	var tunnel []byte
	for _, ipBE := range remoteVTEPs {
		tunnel = append(tunnel, ofclient.BuildActionsSetFieldTunnelID(uint64(vni))...)
		tunnel = append(tunnel, ofclient.BuildActionsSetTunDst(ipBE)...)
		tunnel = append(tunnel, ofclient.BuildActionsOutput(geneveNo)...)
	}
	matchLocal := ofclient.BuildMatchAdvanced(0, uint64(vni), true, 0, true, 0, false, 0, false)
	matchRemote := ofclient.BuildMatchAdvanced(0, uint64(vni), true, 1, true, 0, false, 0, false)
	matchVNI := ofclient.BuildMatchAdvanced(0, uint64(vni), true, 0, false, 0, false, 0, false)

	var plan []t6Flow
	if limited {
		// The tables behind come first: t6 sends nothing into a table that has no flow for the VNI yet.
		plan = append(plan,
			t6Flow{what: "Geneve node meter", table: geneveTable2, priority: 100, match: matchVNI, ins: ofclient.Instructions{Meter: nodeMeterID, Actions: tunnel}, checked: true},
			t6Flow{what: "Geneve lab meter", table: geneveTable1, priority: 100, match: matchVNI, ins: ofclient.Instructions{Meter: labMeterID(vni), GotoTable: geneveTable2}, checked: true},
			t6Flow{what: "t6 local-origin flood", table: 6, priority: 110, match: matchLocal, ins: ofclient.Instructions{Actions: localActions, GotoTable: geneveTable1}, checked: true},
		)
	} else {
		plan = append(plan, t6Flow{what: "t6 local-origin flood", table: 6, priority: 110, match: matchLocal, ins: ofclient.Instructions{Actions: append(append([]byte(nil), localActions...), tunnel...)}})
	}
	// priority=100, metadata=VNI, reg0=1 → local only (no Geneve re-flood)
	return append(plan, t6Flow{what: "t6 remote-origin flood", table: 6, priority: 100, match: matchRemote, ins: ofclient.Instructions{Actions: localActions}})
}
