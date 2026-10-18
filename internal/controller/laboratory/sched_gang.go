package laboratory

import (
	"sort"
	"time"
)

// podNeed is what one pod of a gang requests from a node.
type podNeed struct {
	Key         string
	CPU, Memory int64
}

// gang is what admission decides on as one: the not yet dispatched pods of one deploy group (an independent Lab is its own
// gang, and so is every LabGroup object with its VPN and gateway pods). It starts whole or not at all.
type gang struct {
	key    string
	tenant string
	// reservation names the reservation the gang runs under; empty: unreserved.
	reservation string //nolint:unused // filled from the reservation label by the reservation accounting
	objs        []*schedObject
	// needs are the requests of the pods still to dispatch, in dispatch order.
	needs   []podNeed
	arrival time.Time
}

// fitsGang tries to place every need on a node room, first-fit decreasing by memory, then CPU. It works on a copy: on
// success it returns the rooms after the placement, on failure the rooms as they were.
func fitsGang(rooms []nodeRoom, needs []podNeed) ([]nodeRoom, bool) {
	left := append([]nodeRoom(nil), rooms...)
	order := append([]podNeed(nil), needs...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Memory != order[j].Memory {
			return order[i].Memory > order[j].Memory
		}
		return order[i].CPU > order[j].CPU
	})
	for _, n := range order {
		if n.CPU == 0 && n.Memory == 0 {
			continue // a pod that requests nothing fits anywhere, even with no room listed
		}
		placed := false
		for i := range left {
			if n.Memory <= left[i].Memory && n.CPU <= left[i].CPU {
				left[i].Memory -= n.Memory
				left[i].CPU -= n.CPU
				placed = true
				break
			}
		}
		if !placed {
			return append([]nodeRoom(nil), rooms...), false
		}
	}
	return left, true
}

// subtractFromEmptiest takes the needs off the rooms one by one, each from the room with the most memory left (CPU breaks
// the tie, then the node name): the node the kube scheduler is most likely to give a pod that is not bound yet. What a room cannot give
// is taken from the next emptiest one.
func subtractFromEmptiest(rooms []nodeRoom, needs []amount) []nodeRoom {
	out := append([]nodeRoom(nil), rooms...)
	if len(out) == 0 {
		return out
	}
	order := append([]amount(nil), needs...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].mem != order[j].mem {
			return order[i].mem > order[j].mem
		}
		return order[i].cpu > order[j].cpu
	})
	// emptiest is the room with the most of the owed dimension left (memory when memory is owed, else CPU); -1: none.
	emptiest := func(byMemory bool) int {
		best := -1
		for i := range out {
			have, bestHave := out[i].CPU, int64(0)
			if byMemory {
				have = out[i].Memory
			}
			if best >= 0 {
				bestHave = out[best].CPU
				if byMemory {
					bestHave = out[best].Memory
				}
			}
			switch {
			case have == 0:
			case best < 0 || have > bestHave:
				best = i
			case have == bestHave && (out[i].Memory > out[best].Memory || (out[i].Memory == out[best].Memory && out[i].CPU > out[best].CPU) ||
				(out[i].Memory == out[best].Memory && out[i].CPU == out[best].CPU && out[i].Name < out[best].Name)):
				best = i
			}
		}
		return best
	}
	for _, n := range order {
		// The share a room cannot give is taken from the next emptiest one, memory and CPU each from a room that has it.
		for rest := n; rest.cpu > 0 || rest.mem > 0; {
			best := emptiest(rest.mem > 0)
			if best < 0 {
				if rest.mem > 0 {
					rest.mem = 0 // every room is out of memory: the rest is beyond the nodes
				} else {
					rest.cpu = 0
				}
				continue
			}
			takeMem, takeCPU := min(rest.mem, out[best].Memory), min(rest.cpu, out[best].CPU)
			out[best].Memory -= takeMem
			out[best].CPU -= takeCPU
			rest = amount{cpu: rest.cpu - takeCPU, mem: rest.mem - takeMem}
		}
	}
	return out
}
