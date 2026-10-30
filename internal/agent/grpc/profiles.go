package grpc

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// profileRefusals adds, to the per-item refusals, the items whose devices ask for a profile that cannot run: PROFILE_UNSUPPORTED
// for vm, an unknown profile or one switched off here, PROFILE_UNAVAILABLE when no running node can run it and the autoscaler is
// not known to add one. An item already refused keeps its refusal. Nodes are read once, and only when some device needs them.
func (h *Handler) profileRefusals(ctx context.Context, items []*protobuf.LabItem, variants map[string]*labVariant, refused []error) ([]error, error) {
	policy := h.features.Profiles
	var nodes []corev1.Node
	loaded := false
	for i, it := range items {
		if refused[i] != nil {
			continue
		}
		for _, d := range variants[it.GetVariantId()].spec.Devices {
			if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
				continue
			}
			profile := string(d.Profile.Effective())
			if !policy.Supported(profile) {
				refused[i] = refusal(client.ReasonProfileUnsupported, "device %q: the profile %q is not supported by this laboratory", d.Name, profile)
				break
			}
			if len(policy.Selector(profile)) == 0 {
				continue // runs on any lab node
			}
			if !loaded {
				list, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				nodes, loaded = list.Items, true
			}
			if !policy.AnyNodeRuns(nodes, h.labSelector, h.labTolerations, profile) && !policy.CanScaleUp(profile) {
				refused[i] = refusal(client.ReasonProfileUnavailable, "device %q: no node can run the profile %q now and none can be added", d.Name, profile)
				break
			}
		}
	}
	return refused, nil
}

// nodeProfiles is CapacityResponse.node_profiles: the profiles this laboratory offers and whether they can run.
func (h *Handler) nodeProfiles(ctx context.Context) ([]*protobuf.NodeProfileCapacity, error) {
	policy := h.features.Profiles
	list, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []*protobuf.NodeProfileCapacity
	for _, profile := range []string{nodecap.ProfileContainer, nodecap.ProfileMicroVM} {
		if !policy.Supported(profile) {
			continue
		}
		oh := policy.OverheadOf(profile)
		out = append(out, &protobuf.NodeProfileCapacity{
			Profile:               profile,
			AvailableNow:          policy.AnyNodeRuns(list.Items, h.labSelector, h.labTolerations, profile),
			ScaleUpKnown:          policy.ScaleUpKnown(),
			AvailableAfterScaleUp: policy.CanScaleUp(profile),
			OverheadCpuMillicores: oh.CPU,
			OverheadMemoryBytes:   oh.Memory,
		})
	}
	return out, nil
}
