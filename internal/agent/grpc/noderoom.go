package grpc

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

type nodeRoomCache struct {
	mu   sync.Mutex
	at   time.Time
	list []*protobuf.NodeRoom
}

// nodeRooms is the allocatable and free CPU and memory of every node lab pods can run on (see nodecap.Rooms). It is the
// same for every tenant, so it is read once per capacityTTL. A failed read reports nothing (the caller leaves the
// rooms out) and is not cached.
func (h *Handler) nodeRooms(ctx context.Context) ([]*protobuf.NodeRoom, error) {
	c := &h.roomCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < capacityTTL {
		return c.list, nil
	}
	nodes, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := h.k8s.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rooms := nodecap.Rooms(nodes.Items, pods.Items, h.labSelector, h.labTolerations, h.nodeReserve)
	out := make([]*protobuf.NodeRoom, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, &protobuf.NodeRoom{
			Name:                     r.Name,
			AllocatableCpuMillicores: r.Allocatable.CPU, AllocatableMemoryBytes: r.Allocatable.Memory,
			FreeCpuMillicores: r.Free.CPU, FreeMemoryBytes: r.Free.Memory,
		})
	}
	c.at, c.list = time.Now(), out
	return out, nil
}
