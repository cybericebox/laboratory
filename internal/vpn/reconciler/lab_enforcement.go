//go:build linux

package reconciler

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// dropPublishInterval is how often the count of dropped packets of a lab is written, when it changed (it is a metric for the
// administrator, not a signal; the confirmation of the block is written at once).
const dropPublishInterval = 30 * time.Second

// dropCounter is the part of the packet filter that counts what the block of lab-started connections dropped.
type dropCounter interface {
	LabDropCounts(ctx context.Context) (map[string]uint64, error)
}

// enforcementView says, per VPN gateway leg, whether this gateway program has applied the block, and what it dropped.
type enforcementView struct {
	blocked bool
	dropped int64
}

// labEnforcement computes what is published on the LabVPN of every lab of the snapshot. The block counts as applied only for a lab whose
// switch is off, that has its interface, and only once the access rules of the snapshot are in the kernel (applied).
func labEnforcement(applied bool, labs map[string]vpn.LabAccessSnapshot, drops map[string]uint64) map[string]enforcementView {
	out := make(map[string]enforcementView, len(labs))
	for name, l := range labs {
		v := enforcementView{}
		if applied && !l.LabInitiated && l.Interface != "" {
			v.blocked = true
			v.dropped = int64(min(drops[l.Interface], 1<<62))
		}
		out[name] = v
	}
	return out
}

// publishLabEnforcement writes the confirmation of the block (at once) and the drop count (at most every dropPublishInterval) on the
// LabVPN of each lab. A VPN pod that runs an older program never calls this, so its labs are never confirmed.
func (r *AccessReconciler) publishLabEnforcement(ctx context.Context, labs map[string]vpn.LabAccessSnapshot, legs []lab.LabVPN) error {
	var drops map[string]uint64
	if dc, ok := r.IPT.(dropCounter); ok {
		var err error
		if drops, err = dc.LabDropCounts(ctx); err != nil {
			drops = nil // the confirmation does not depend on the metric
		}
	}
	views := labEnforcement(r.applied, labs, drops)
	if r.dropPublished == nil {
		r.dropPublished = map[string]time.Time{}
	}
	now := time.Now()
	for i := range legs {
		leg := &legs[i]
		if !leg.DeletionTimestamp.IsZero() {
			continue
		}
		v, ok := views[leg.Name]
		if !ok {
			continue
		}
		st := &leg.Status
		changed := st.LabInitiatedBlocked != v.blocked
		if v.dropped != st.LabInitiatedDropped && now.Sub(r.dropPublished[leg.Name]) >= dropPublishInterval {
			changed = true
		}
		if !changed {
			continue
		}
		base := leg.DeepCopy()
		st.LabInitiatedBlocked = v.blocked
		if now.Sub(r.dropPublished[leg.Name]) >= dropPublishInterval || !v.blocked {
			st.LabInitiatedDropped = v.dropped
			r.dropPublished[leg.Name] = now
		}
		if err := r.Status().Patch(ctx, leg, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}
