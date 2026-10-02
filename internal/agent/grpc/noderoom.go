package grpc

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/nodecap"
)

type deviceRoomCache struct {
	mu   sync.Mutex
	at   time.Time
	room nodecap.Amount
	ok   bool
}

// largestDevice is the largest device the agent can place: per resource the largest allocatable of a lab node net of the
// platform reserve (nodecap.LargestDevice). The per-node numbers stay here, only this one amount leaves the agent: the
// cluster layout is never shown to a tenant. It is the same for every tenant, so it is read once per capacityTTL; ok is
// false when it cannot be read or no node is schedulable.
func (h *Handler) largestDevice(ctx context.Context) (nodecap.Amount, bool) {
	c := &h.roomCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < capacityTTL {
		return c.room, c.ok
	}
	nodes, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nodecap.Amount{}, false
	}
	rooms := nodecap.Rooms(nodes.Items, nil, h.labSelector, h.labTolerations, h.nodeReserve)
	c.at, c.room, c.ok = time.Now(), nodecap.LargestDevice(rooms), len(rooms) > 0
	return c.room, c.ok
}
