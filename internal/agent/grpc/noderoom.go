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
	// largest is the largest device: per resource the largest allocatable of one node.
	largest nodecap.Amount
	// ok is false when no node is schedulable or the nodes could not be read.
	ok bool
}

type deviceRoomCache struct {
	mu   sync.Mutex
	at   time.Time
	view roomView
}

// room reads the lab nodes (nodecap.Rooms with no pods: only the allocatable counts) and caches the view for a few
// seconds: it is the same for every tenant.
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
	rooms := nodecap.Rooms(nodes.Items, nil, h.labSelector, h.labTolerations, h.nodeReserve)
	v := roomView{largest: nodecap.LargestDevice(rooms), ok: len(rooms) > 0}
	for _, r := range rooms {
		v.total.CPU += r.Allocatable.CPU
		v.total.Memory += r.Allocatable.Memory
	}
	c.at, c.view = time.Now(), v
	return v
}

// SetPackingReserve sets the hidden packing reserve (percent, 0-49): the capacity reported to tenants is net of it, because
// nodes do not share memory and devices leave gaps between them, so the last part of a cluster cannot be packed full. A
// tenant never sees the reserve.
func (h *Handler) SetPackingReserve(percent int) { h.packingReserve = percent }

// reported is the capacity the tenant is told about: its quota, or the real room of the cluster when it has none,
// whichever is smaller, net of the packing reserve. The quota the operator enforces is not changed: the platform plans inside
// the smaller reported number and never reaches the hard cap in a way that leaves holes.
func (h *Handler) reported(ctx context.Context, l tenant.Limits) tenant.Limits {
	v := h.room(ctx)
	keep := int64(100 - h.packingReserve)
	apply := func(has bool, limit, real int64) (bool, int64) {
		switch {
		case has && v.ok:
			limit = min(limit, real)
		case !has && v.ok:
			has, limit = true, real
		case !has:
			return false, 0
		}
		return true, limit * keep / 100
	}
	l.HasCPU, l.CPU = apply(l.HasCPU, l.CPU, v.total.CPU)
	l.HasMemory, l.Memory = apply(l.HasMemory, l.Memory, v.total.Memory)
	return l
}
