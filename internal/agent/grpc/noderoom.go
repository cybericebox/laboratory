package grpc

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/internal/tenant"
)

// roomView is what the agent derives from the lab nodes and lets out: the room all of them give together and the largest
// device one of them can hold, both net of the platform reserve. The nodes themselves, their names and their load stay here:
// the cluster layout is never shown to a tenant.
type roomView struct {
	// total is the sum of the allocatable of the schedulable lab nodes net of the platform reserve.
	total nodecap.Amount
	// largest is the largest device: per resource the most one lab node gives with nothing on it, net of the packing reserve
	// too (nodecap.LargestDevice). An upper bound per resource; the operator fails a pod only when no node holds both.
	largest nodecap.Amount
	// ok is false when no node is schedulable or the nodes could not be read.
	ok bool
}

type deviceRoomCache struct {
	mu   sync.Mutex
	at   time.Time
	view roomView
}

// room reads the lab nodes and the pods on them and caches the view for a few seconds: it is the same for every tenant. The
// total is what the operator counts as room: per node the allocatable less the platform reserve, the requests of the
// non-lab pods and the packing reserve (nodecap.FreeForLabs). The largest device is what one node gives with no load, net
// of the same reserves (nodecap.LargestDevice), an upper bound per resource.
func (h *Handler) room(ctx context.Context) roomView {
	c := &h.roomCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < capacityTTL {
		return c.view
	}
	nodes, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return roomView{}
	}
	pods, err := h.k8s.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return roomView{}
	}
	rooms := nodecap.Rooms(nodes.Items, nil, h.labSelector, h.labTolerations, h.nodeReserve)
	v := roomView{largest: nodecap.LargestDevice(nodes.Items, h.labSelector, h.labTolerations, h.nodeReserve, h.packingReserve), ok: len(rooms) > 0}
	free := nodecap.FreeForLabs(nodes.Items, pods.Items, h.labSelector, h.labTolerations, h.nodeReserve, h.packingReserve)
	if count, room, ok := h.virtualRoom(ctx, nodes.Items); ok && count > 0 {
		// The nodes still to come up to the static maximum: the operator admits onto them (its breaker aside), so the tenants
		// plan against them too.
		free = append(free, nodecap.VirtualRooms(count, room)...)
		v.largest = nodecap.Amount{CPU: max(v.largest.CPU, room.CPU), Memory: max(v.largest.Memory, room.Memory)}
		v.ok = true
	}
	for _, f := range free {
		v.total.CPU += f.CPU
		v.total.Memory += f.Memory
	}
	c.at, c.view = time.Now(), v
	return v
}

// virtualRoom is the number of lab nodes still to come up to the static maximum and the room for labs on one of them; ok false
// without a maximum, or when the DaemonSets could not be read (the room of the nodes that exist is reported then).
func (h *Handler) virtualRoom(ctx context.Context, nodes []corev1.Node) (count int, room nodecap.NodeFree, ok bool) {
	if h.maximum.Nodes <= 0 {
		return 0, nodecap.NodeFree{}, false
	}
	dss, err := h.k8s.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nodecap.NodeFree{}, false
	}
	count, room = h.maximum.Virtual(nodes, dss.Items, h.labSelector, h.labTolerations, h.nodeReserve, h.packingReserve)
	return count, room, true
}

// SetMaximum sets the static maximum capacity of the lab nodes (the operator's, the same chart values): the room reported to
// tenants counts the nodes still to come up to it.
func (h *Handler) SetMaximum(m nodecap.Maximum) { h.maximum = m }

// SetPackingReserve sets the hidden packing reserve: the room reported to tenants is net of it, because nodes do not share
// memory and devices leave gaps between them, so the last part of a node cannot be packed full. A tenant never sees the reserve.
func (h *Handler) SetPackingReserve(r nodecap.PackingReserve) { h.packingReserve = r }

// reported is the capacity the tenant is told about: its quota net of the packing reserve, or the real room of the cluster
// (already net of it per node) when it has none, whichever is smaller. The quota the operator enforces is not changed: the
// platform plans inside the smaller reported number and never reaches the hard cap in a way that leaves holes.
func (h *Handler) reported(ctx context.Context, l tenant.Limits) tenant.Limits {
	v := h.room(ctx)
	quotaCPU, quotaMem := h.packingReserve.Apply(l.CPU, l.Memory)
	apply := func(has bool, quota, real int64) (bool, int64) {
		switch {
		case has && v.ok:
			return true, min(quota, real)
		case !has && v.ok:
			return true, real
		case !has:
			return false, 0
		}
		return true, quota
	}
	l.HasCPU, l.CPU = apply(l.HasCPU, quotaCPU, v.total.CPU)
	l.HasMemory, l.Memory = apply(l.HasMemory, quotaMem, v.total.Memory)
	return l
}
