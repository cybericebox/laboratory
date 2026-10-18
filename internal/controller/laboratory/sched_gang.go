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

// decreasing orders the needs largest first: by memory, then CPU (stable, so equal needs keep their order).
func decreasing(needs []podNeed) []podNeed {
	order := append([]podNeed(nil), needs...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Memory != order[j].Memory {
			return order[i].Memory > order[j].Memory
		}
		return order[i].CPU > order[j].CPU
	})
	return order
}

// fitsGang tries to place every need on a node room, first-fit decreasing by memory, then CPU. Each pod goes whole onto one
// room: its CPU and memory come from the same node. It works on a copy: on success it returns the rooms after the placement,
// on failure the rooms as they were.
func fitsGang(rooms []nodeRoom, needs []podNeed) ([]nodeRoom, bool) {
	left := append([]nodeRoom(nil), rooms...)
	for _, n := range decreasing(needs) {
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

// placeEach places the needs one by one, each whole onto one room, first-fit decreasing as fitsGang, and returns the rooms
// after them. A need that fits no room is left out: that pod stays pending, so it takes no room yet.
func placeEach(rooms []nodeRoom, needs []podNeed) []nodeRoom {
	left := append([]nodeRoom(nil), rooms...)
	for _, n := range decreasing(needs) {
		if l, ok := fitsGang(left, []podNeed{n}); ok {
			left = l
		}
	}
	return left
}

// without takes the needs of pods bound to known nodes off the rooms of those nodes (floored at zero); a node not in rooms is
// skipped.
func without(rooms []nodeRoom, bound map[string]amount) []nodeRoom {
	out := append([]nodeRoom(nil), rooms...)
	for i := range out {
		used := bound[out[i].Name]
		out[i].CPU, out[i].Memory = max(out[i].CPU-used.cpu, 0), max(out[i].Memory-used.mem, 0)
	}
	return out
}
