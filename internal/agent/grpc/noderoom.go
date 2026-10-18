package grpc

import (
	"context"
	"sync"
	"time"

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
	// too (nodecap.LargestDevice, the operator's never-fits limit).
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
// of the same reserves (nodecap.LargestDevice), so a device the platform accepts is one the operator can place.
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
	for _, f := range nodecap.FreeForLabs(nodes.Items, pods.Items, h.labSelector, h.labTolerations, h.nodeReserve, h.packingReserve) {
		v.total.CPU += f.CPU
		v.total.Memory += f.Memory
	}
	c.at, c.view = time.Now(), v
	return v
}

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
