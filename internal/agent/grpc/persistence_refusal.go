package grpc

import (
	"context"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// A lab that asks for state persistence is not created or started on a cluster whose nodes cannot keep it: it would run without the
// saving it was told it has. The refusal is PERSISTENCE_UNAVAILABLE and names only the fixed reason of GetFeatures.

// labSpecUsesPersistence says some device asks for persistence.
func labSpecUsesPersistence(spec *laboratoryv1alpha1.LabSpec) bool {
	for i := range spec.Devices {
		if p := spec.Devices[i].Persistence; p != nil && p.Enabled {
			return true
		}
	}
	return false
}

// persistenceUnavailable is the reason (`<CODE>:<n>/<m>`) the lab nodes give for not keeping state, "" when they confirm it, when the
// cluster is not configured for persistence (the spec check refuses that), or when no node has reported yet.
func (h *Handler) persistenceUnavailable(ctx context.Context) (string, error) {
	if !h.features.StatePersistence {
		return "", nil
	}
	v, err := h.nodeVerdict(ctx, true, false)
	if err != nil || v.checked == 0 {
		return "", err
	}
	return v.persistenceReason, nil
}

func persistenceRefusal(reason string) error {
	return refusal(client.ReasonPersistenceUnavailable, "state persistence is not available on this cluster: %s", reason)
}

// persistenceRefusals refuses, among the items of a create, those whose lab asks for persistence, when the nodes cannot keep it.
func (h *Handler) persistenceRefusals(ctx context.Context, items []*protobuf.LabItem, variants map[string]*labVariant, refused []error) ([]error, error) {
	var reason string
	asked := false
	for i, it := range items {
		if refused[i] != nil || !labSpecUsesPersistence(&variants[it.GetVariantId()].spec) {
			continue
		}
		if !asked {
			var err error
			if reason, err = h.persistenceUnavailable(ctx); err != nil {
				return nil, err
			}
			asked = true
		}
		if reason != "" {
			refused[i] = persistenceRefusal(reason)
		}
	}
	return refused, nil
}

// refuseStartWithoutPersistence is the same for the start of an existing lab.
func (h *Handler) refuseStartWithoutPersistence(ctx context.Context, l *laboratoryv1alpha1.Lab) error {
	if !labSpecUsesPersistence(&l.Spec) {
		return nil
	}
	reason, err := h.persistenceUnavailable(ctx)
	if err != nil || reason == "" {
		return err
	}
	return persistenceRefusal(reason)
}
