package laboratory

import (
	"math"
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
	// reservation names the reservation the gang runs under; empty: unreserved. mixedReservation says its objects carry
	// different reservation labels (or some none): the gang is refused whole (gangReservation).
	reservation      string
	mixedReservation bool
	objs             []*schedObject
	// needs are the requests of the pods still to dispatch, in dispatch order.
	needs   []podNeed
	arrival time.Time
}

// emptyReservation says an object of the gang carries an empty reservation label.
func (g *gang) emptyReservation() bool {
	for _, o := range g.objs {
		if o.labelled && o.reservation == "" {
			return true
		}
	}
	return false
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

// started says some pod of the gang is out and some still waits: the gang is admitted.
func (g *gang) started() bool {
	out, waiting := false, false
	for _, o := range g.objs {
		for _, p := range o.pods {
			if p.dispatched() {
				out = true
			} else {
				waiting = true
			}
		}
	}
	return out && waiting
}

// retry says the gang was withdrawn and waits for its retry time: the latest retry time of its waiting pods while it is after
// now, with the attempt of the withdrawal.
func (g *gang) retry(now time.Time) (at time.Time, attempt int32, wait bool) {
	for _, o := range g.objs {
		for _, p := range o.pods {
			if p.dispatched() || !p.retryAfter.After(now) {
				continue
			}
			wait = true
			if p.retryAfter.After(at) {
				at = p.retryAfter
			}
			attempt = max(attempt, p.withdrawals)
		}
	}
	return at, attempt, wait
}

// waitingSince is when the gang started waiting in this queue episode: the earliest time one of its waiting pods entered the
// queue (a lab resumed after a stop is queued again then), at the earliest its arrival; the arrival when no waiting pod has a
// queue time.
func (g *gang) waitingSince() time.Time {
	var since time.Time
	for _, o := range g.objs {
		for _, p := range o.pods {
			if p.dispatched() || p.queuedAt.IsZero() {
				continue
			}
			if since.IsZero() || p.queuedAt.Before(since) {
				since = p.queuedAt
			}
		}
	}
	if since.IsZero() || since.Before(g.arrival) {
		return g.arrival
	}
	return since
}

// reservationUsage is what the dispatched pods of a reservation request (used) against what it reserves (amount; zero for an
// invalid reservation), the tenant it belongs to, whether its window is open now, and how many groups have pods out under it.
type reservationUsage struct {
	used, amount amount
	tenant       string
	active       bool
	groups       int32
}

// share is the satisfied share of a reservation, used/amount, for memory and CPU. A reservation that reserves nothing of a
// resource (an invalid one counts as zero) has an infinite share of it: it goes after every valid one.
func (u reservationUsage) share() (mem, cpu float64) {
	ratio := func(used, amount int64) float64 {
		if amount <= 0 {
			return math.Inf(1)
		}
		return float64(used) / float64(amount)
	}
	return ratio(u.used.mem, u.amount.mem), ratio(u.used.cpu, u.amount.cpu)
}

// shareOf is the share of a reservation; one not in the usage has used nothing yet.
func shareOf(usage map[string]reservationUsage, name string) (mem, cpu float64) {
	u, ok := usage[name]
	if !ok {
		return 0, 0
	}
	return u.share()
}

// fairness is what the plan needs beyond the cluster to choose the next gang: the usage of each reservation (a reservation
// not in it has used nothing), the clock and the aging threshold (zero: no aging).
type fairness struct {
	usage map[string]reservationUsage
	now   time.Time
	aging time.Duration
	// nodeTimeout is the node timeout of the gang commit, for the message of a withdrawn gang.
	nodeTimeout time.Duration
}

// byAge orders gangs by arrival, then tenant, then key.
func byAge(a, b *gang) bool {
	if !a.arrival.Equal(b.arrival) {
		return a.arrival.Before(b.arrival)
	}
	if a.tenant != b.tenant {
		return a.tenant < b.tenant
	}
	return a.key < b.key
}

// gangOrder is the order in which gangs are offered the room. Admitted gangs (some pod out, some waiting) keep their strict
// order (arrival, tenant, key) ahead of the rest. Then the new gangs that run under a reservation, the lowest satisfied share
// first (memory share, then CPU), then arrival, tenant, key. The unreserved gangs come last in the order they came in (the
// conveyor: groups by arrival, tenant, key, then the independent labs by their priority and size). A gang that does not fit
// is passed over by the plan (backfill), so the order only says who is offered the room first.
func gangOrder(gangs []*gang, usage map[string]reservationUsage) []*gang {
	class := func(g *gang) int {
		switch {
		case g.started():
			return 0
		case g.reservation != "":
			return 1
		}
		return 2
	}
	out := append([]*gang(nil), gangs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ca, cb := class(a), class(b)
		if ca != cb {
			return ca < cb
		}
		switch ca {
		case 0:
			return byAge(a, b)
		case 1:
			am, ac := shareOf(usage, a.reservation)
			bm, bc := shareOf(usage, b.reservation)
			if am != bm {
				return am < bm
			}
			if ac != bc {
				return ac < bc
			}
			return byAge(a, b)
		}
		return false
	})
	return out
}

// placed is the room a placement took on each node: the rooms before it less the rooms after it.
func placed(before, after []nodeRoom) map[string]amount {
	out := map[string]amount{}
	for i := range before {
		out[before[i].Name] = out[before[i].Name].add(amount{cpu: before[i].CPU - after[i].CPU, mem: before[i].Memory - after[i].Memory})
	}
	return out
}
