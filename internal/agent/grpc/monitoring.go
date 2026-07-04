package grpc

import (
	"context"
	"errors"
	"io"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Ping is a trivial liveness check used by the manager to verify the agent
// connection is alive.
func (h *Handler) Ping(_ context.Context, _ *protobuf.Empty) (*protobuf.Empty, error) {
	return &protobuf.Empty{}, nil
}

// snapshot builds a MonitoringUpdate covering all LabGroups (cluster-scoped)
// and, for each group with a provisioned namespace, its Labs and
// LabGroupClients (namespace-scoped).
func (h *Handler) snapshot(ctx context.Context) (*protobuf.MonitoringUpdate, error) {
	groups, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	upd := &protobuf.MonitoringUpdate{}
	for i := range groups.Items {
		g := &groups.Items[i]
		upd.Groups = append(upd.Groups, labGroupToProto(g))
		ns := g.Status.Namespace
		if ns == "" {
			continue
		}
		labs, err := h.cs.LaboratoryV1alpha1().Labs(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for j := range labs.Items {
				upd.Labs = append(upd.Labs, labToProto(&labs.Items[j]))
			}
		}
		clients, err := h.cs.LaboratoryV1alpha1().LabGroupClients(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for j := range clients.Items {
				upd.Clients = append(upd.Clients, clientToProto(&clients.Items[j], nil))
			}
		}
	}
	return upd, nil
}

// Monitoring implements the bidirectional monitoring stream: each Recv from
// the client is treated as a poll tick, triggering a fresh snapshot of the
// cluster state which is sent back as a single MonitoringUpdate. The loop
// exits cleanly when the client closes the stream (io.EOF) or the stream
// context is cancelled.
func (h *Handler) Monitoring(stream protobuf.LabManager_MonitoringServer) error {
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		upd, err := h.snapshot(stream.Context())
		if err != nil {
			return err
		}
		if err := stream.Send(upd); err != nil {
			return err
		}
	}
}
