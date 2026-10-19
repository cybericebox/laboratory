package laboratory

import (
	"fmt"
	"math/bits"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// This file accounts pods against their reservations and keeps the unused part of every active reservation out of the room
// offered to the others. See planSchedule for where it enters the placement simulation.

// dispatchedPod is a pod that is out (Starting, Started or Failed) as the reservation ledger counts it.
type dispatchedPod struct {
	// reservation is the reservation label of the pod's object; empty: unreserved.
	reservation string
	tenant      string
	// group is the gang the pod belongs to: the deploy group within the tenant, or the object of an independent one.
	group string
	need  amount
}

// reservationAmount is what a reservation reserves. One that is invalid reserves nothing (reservationProblem).
func reservationAmount(r *laboratoryv1alpha1.Reservation) amount {
	if reservationProblem(r) != "" {
		return amount{}
	}
	return amount{cpu: r.Spec.Resources.Cpu().MilliValue(), mem: r.Spec.Resources.Memory().Value()}
}

// reservationProblem says why the resources of a reservation are invalid, "" when they are not: a resource other than cpu and
// memory, or cpu or memory missing, zero or negative (CEL cannot check the values within its cost budget, so the operator
// does; the ReservationReconciler shows it as the Invalid condition).
func reservationProblem(r *laboratoryv1alpha1.Reservation) string {
	for name := range r.Spec.Resources {
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
			return fmt.Sprintf("resources name %q; only cpu and memory are reserved", name)
		}
	}
	if q, ok := r.Spec.Resources[corev1.ResourceCPU]; !ok || q.Sign() <= 0 || q.MilliValue() <= 0 {
		return "resources.cpu must be positive"
	}
	if q, ok := r.Spec.Resources[corev1.ResourceMemory]; !ok || q.Sign() <= 0 {
		return "resources.memory must be positive"
	}
	return ""
}

// reservationActive says the window of the reservation is open: from <= now < to, it is not being deleted and it has not
// ended early (status.phase Ended inside the window: its tenant is gone).
func reservationActive(r *laboratoryv1alpha1.Reservation, now time.Time) bool {
	return r.DeletionTimestamp == nil && r.Status.Phase != laboratoryv1alpha1.ReservationPhaseEnded && !now.Before(r.Spec.From.Time) && now.Before(r.Spec.To.Time)
}

// gangReservation is the reservation of a gang: the label all its objects carry (empty: unreserved). A gang is under one
// reservation or none: when its objects disagree (two labels, or a label on some objects only) mixed is true, and the gang is
// refused with ReservationMissing and takes no withheld room; nothing new of it is counted (ledgerPods).
func gangReservation(objs []*schedObject) (name string, mixed bool) {
	for i, o := range objs {
		if i == 0 {
			name = o.reservation
		} else if o.reservation != name {
			return name, true
		}
	}
	return name, false
}

// ledgerPods are the dispatched pods of the objects as the ledger counts them: each under the reservation of its gang (the
// objects of a deploy group within a tenant; a LabGroup and an independent lab alone), as the plan builds the gangs, so the
// ledger and the admission gate agree. The pods of a gang with mixed labels (refused whole) count under their own object's label.
func ledgerPods(objs []*schedObject) []dispatchedPod {
	gangOf := func(o *schedObject) string {
		if o.isGroup() || o.group == "" {
			return o.id
		}
		return groupKey(objTenant(o), o.group)
	}
	members := map[string][]*schedObject{}
	for _, o := range objs {
		members[gangOf(o)] = append(members[gangOf(o)], o)
	}
	var out []dispatchedPod
	for _, o := range objs {
		res, mixed := gangReservation(members[gangOf(o)])
		if mixed {
			// refused whole, so nothing new starts; what is out counts under its own label, so a label changed late never lets
			// a reservation admit again what already runs
			res = o.reservation
		}
		// the groups of a reservation are its deploy groups (a LabGroup of the group is one with its labs)
		group := o.id
		if o.group != "" {
			group = groupKey(objTenant(o), o.group)
		}
		for _, p := range o.pods {
			if p.dispatched() {
				out = append(out, dispatchedPod{reservation: res, tenant: p.tenant, group: group, need: p.need})
			}
		}
	}
	return out
}

// reservationLedger is the usage of every reservation: the requests of the dispatched pods that run under it (a pod counts only
// for a reservation of its own tenant), what it reserves, its tenant, whether it is active now, and how many groups have pods out
// under it.
func reservationLedger(res []laboratoryv1alpha1.Reservation, pods []dispatchedPod, now time.Time) map[string]reservationUsage {
	out := make(map[string]reservationUsage, len(res))
	for i := range res {
		r := &res[i]
		out[r.Name] = reservationUsage{amount: reservationAmount(r), tenant: r.Spec.Tenant, active: reservationActive(r, now)}
	}
	groups := map[string]map[string]bool{}
	for _, p := range pods {
		u, ok := out[p.reservation]
		if p.reservation == "" || !ok || u.tenant != p.tenant {
			continue
		}
		u.used = u.used.add(p.need)
		if groups[p.reservation] == nil {
			groups[p.reservation] = map[string]bool{}
		}
		if !groups[p.reservation][p.group] {
			groups[p.reservation][p.group] = true
			u.groups++
		}
		out[p.reservation] = u
	}
	return out
}

// reservationPass is the reservation side of one placement simulation.
type reservationPass struct {
	usage map[string]reservationUsage
	// pending is what each reservation has taken in the pass: its dispatched pods, the remainders held for its admitted gangs
	// and the gangs admitted now.
	pending map[string]amount
	// withheld is the room kept for the unused part of each active reservation, by node; the others never place on it.
	withheld map[string]map[string]amount
	// shortfall is what of the unused part of a reservation found no room on the nodes.
	shortfall map[string]amount
	// order is, for each reservation, the nodes in the order its room was withheld on them (the emptiest first).
	order map[string][]string
}

func newReservationPass(usage map[string]reservationUsage) *reservationPass {
	r := &reservationPass{usage: usage, pending: map[string]amount{}, withheld: map[string]map[string]amount{}, shortfall: map[string]amount{},
		order: map[string][]string{}}
	for name, u := range usage {
		r.pending[name] = u.used
	}
	return r
}

// gate says why a gang under a reservation may not take more now (a Wait reason); empty: it may, the need within what is left of
// the reservation. An unreserved gang always may.
func (r *reservationPass) gate(gg *gang, need amount) string {
	if gg.reservation == "" && !gg.mixedReservation {
		return ""
	}
	u, ok := r.usage[gg.reservation]
	switch {
	case gg.mixedReservation || !ok || u.tenant != gg.tenant:
		return laboratoryv1alpha1.WaitReservationMissing
	case !u.active:
		return laboratoryv1alpha1.WaitReservationNotActive
	}
	total := r.pending[gg.reservation].add(need)
	if u.amount.cpu <= 0 || u.amount.mem <= 0 || total.cpu > u.amount.cpu || total.mem > u.amount.mem {
		return laboratoryv1alpha1.WaitReservationFull
	}
	return ""
}

// take counts a gang's need against its reservation for the rest of the pass.
func (r *reservationPass) take(name string, need amount) {
	if name != "" {
		r.pending[name] = r.pending[name].add(need)
	}
}

// unused is what an active reservation has not taken yet; nothing for one that is not active or reserves nothing.
func (r *reservationPass) unused(name string) amount {
	u, ok := r.usage[name]
	if !ok || !u.active {
		return amount{}
	}
	return u.amount.sub(r.pending[name]).floorZero()
}

// withhold keeps the unused part of every active reservation on the rooms, as abstract room by node, in the given order (the
// reservations that are not in it come after, by name). A reservation is placed like one more gang that is offered the rooms
// after the ones before it: its unused part goes onto the emptiest node (memory, then CPU) as far as that node holds it, keeping
// its CPU to memory ratio, and the rest onto the next emptiest; what no node holds is its shortfall.
func (r *reservationPass) withhold(rooms []nodeRoom, order []string) {
	seen := map[string]bool{}
	var names []string
	for _, n := range order {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	var rest []string
	for n := range r.usage {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	names = append(names, rest...)

	left := append([]nodeRoom(nil), rooms...)
	for _, name := range names {
		rem := r.unused(name)
		if rem == (amount{}) {
			continue
		}
		byRoom := make([]int, len(left))
		for i := range byRoom {
			byRoom[i] = i
		}
		sort.SliceStable(byRoom, func(a, b int) bool {
			x, y := left[byRoom[a]], left[byRoom[b]]
			if x.Memory != y.Memory {
				return x.Memory > y.Memory
			}
			return x.CPU > y.CPU
		})
		kept := map[string]amount{}
		for _, i := range byRoom {
			r.order[name] = append(r.order[name], left[i].Name)
			if rem == (amount{}) {
				break
			}
			t := proportional(rem, amount{cpu: left[i].CPU, mem: left[i].Memory})
			if t == (amount{}) {
				continue
			}
			left[i].CPU -= t.cpu
			left[i].Memory -= t.mem
			kept[left[i].Name] = kept[left[i].Name].add(t)
			rem = rem.sub(t)
		}
		r.withheld[name] = kept
		if rem != (amount{}) {
			r.shortfall[name] = rem
		}
	}
}

// proportional is the part of want a room holds with the CPU to memory ratio of want kept (rounded down); a resource want does
// not ask for does not limit it.
func proportional(want, room amount) amount {
	switch {
	case want.cpu <= 0:
		return amount{mem: min(want.mem, room.mem)}
	case want.mem <= 0:
		return amount{cpu: min(want.cpu, room.cpu)}
	case room.cpu >= want.cpu && room.mem >= want.mem:
		return want
	case room.cpu <= 0 || room.mem <= 0:
		return amount{}
	}
	// room.mem/want.mem against room.cpu/want.cpu: the smaller share binds.
	if less(room.mem, want.cpu, room.cpu, want.mem) {
		return amount{cpu: mulDiv(want.cpu, room.mem, want.mem), mem: room.mem}
	}
	return amount{cpu: room.cpu, mem: mulDiv(want.mem, room.cpu, want.cpu)}
}

// less says a*b < c*d for non-negative values, without overflow.
func less(a, b, c, d int64) bool {
	h1, l1 := bits.Mul64(uint64(a), uint64(b))
	h2, l2 := bits.Mul64(uint64(c), uint64(d))
	return h1 < h2 || h1 == h2 && l1 < l2
}

// mulDiv is a*b/c rounded down for non-negative values with a result that fits (b <= c).
func mulDiv(a, b, c int64) int64 {
	h, l := bits.Mul64(uint64(a), uint64(b))
	q, _ := bits.Div64(h, l, uint64(c))
	return int64(q)
}

// total is the room withheld for every reservation, by node; except names one whose room is left out.
func (r *reservationPass) total(except string) map[string]amount {
	out := map[string]amount{}
	for name, kept := range r.withheld {
		if name == except {
			continue
		}
		for n, a := range kept {
			out[n] = out[n].add(a)
		}
	}
	return out
}

// view is the room offered to a gang: the rooms less the room withheld for every reservation and less the room kept for the aged
// gang (nil: none), plus the room withheld for the gang's own reservation (own; empty: none).
func (r *reservationPass) view(rooms []nodeRoom, own string, aged map[string]amount) []nodeRoom {
	free := without(rooms, r.total(""))
	if aged != nil {
		free = without(free, aged)
	}
	for i := range free {
		a := r.withheld[own][free[i].Name]
		free[i].CPU += a.cpu
		free[i].Memory += a.mem
	}
	return free
}

// own is the room withheld for a reservation alone, in the order of the rooms.
func (r *reservationPass) own(rooms []nodeRoom, name string) []nodeRoom {
	out := make([]nodeRoom, len(rooms))
	for i := range rooms {
		a := r.withheld[name][rooms[i].Name]
		out[i] = nodeRoom{Name: rooms[i].Name, CPU: a.cpu, Memory: a.mem}
	}
	return out
}

// use records a gang of the reservation placed now (where by node, need in all): the room withheld for the reservation shrinks by
// what the gang took of it. When more is still withheld than the reservation has left (the gang took free room too), only the
// excess is released, per resource, from the node withheld last (the least empty) back, so the reservation keeps every unit it
// still needs. A node may then keep an odd CPU to memory ratio: that is kept rather than releasing room the reservation needs.
func (r *reservationPass) use(name string, where map[string]amount, need amount) {
	if name == "" {
		return
	}
	r.take(name, need)
	kept := r.withheld[name]
	if kept == nil {
		return
	}
	var sum amount
	for n, a := range kept {
		a = a.sub(where[n]).floorZero()
		kept[n] = a
		sum = sum.add(a)
	}
	over := sum.sub(r.unused(name)).floorZero()
	order := r.order[name]
	for i := len(order) - 1; i >= 0 && over != (amount{}); i-- {
		n := order[i]
		cut := amount{cpu: min(over.cpu, kept[n].cpu), mem: min(over.mem, kept[n].mem)}
		kept[n] = kept[n].sub(cut)
		over = over.sub(cut)
	}
}

// snapshot copies the withheld room, for the plan.
func (r *reservationPass) snapshot() map[string]map[string]amount {
	out := make(map[string]map[string]amount, len(r.withheld))
	for name, kept := range r.withheld {
		c := map[string]amount{}
		for n, a := range kept {
			if a != (amount{}) {
				c[n] = a
			}
		}
		out[name] = c
	}
	return out
}
