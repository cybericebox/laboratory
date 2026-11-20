package devicestate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// What the node does with a push the registry gate refused by a rule of its own (the limits live in the gate: it measures what it sees
// and does not trust what the node says about its own sizes). A refusal is not a failure of the device:
//
//   - 429 with Retry-After (the least interval between pushes, the push budget, the limit of exit snapshots): the snapshot is deferred for
//     the time the gate named, and taken again then; nothing is lost, the next snapshot has it;
//   - 507 CIB_REGISTRY_FULL: the last good snapshot stays; the lab status warning says why, and the live snapshot is tried again later;
//   - 403 CIB_TENANT_QUOTA: the tenant's snapshots would take more than its quota: the layer is not retried as it is, and the warning says so.
const (
	// defaultGateRetry is the wait of a 429 that named none, registryFullRetry the wait after a full registry.
	defaultGateRetry = time.Minute
	minGateRetry     = time.Second
	registryFullWait = 5 * time.Minute
)

// gateRefusal maps the error of a push onto the deferral and the lab status warning. handled says the error was a refusal of the gate and
// result is what the snapshot returns; otherwise the caller treats err as any failure of a push.
func (t *tracked) gateRefusal(ctx context.Context, err error, required, freeze bool, digest string) (handled bool, result error) {
	var ref *snapshot.Refusal
	if !errors.As(err, &ref) {
		return false, nil
	}
	after := ref.RetryAfter
	switch {
	case ref.Status == 429:
		if after <= 0 {
			after = defaultGateRetry
		}
		if after < minGateRetry {
			after = minGateRetry
		}
		return true, &errDeferred{after: after, reason: ref.Error()}
	case ref.Code == snapshot.CodeTenantQuota:
		if required {
			return true, fmt.Errorf("%w: %s", snapshot.ErrQuota, ref.Message)
		}
		t.lastDiff = digest // the same layer is not offered again
		t.warn(ctx, fmt.Sprintf("%v: %s", snapshot.ErrQuota, ref.Message))
		return true, nil
	case ref.Code == snapshot.CodeRegistryFull || ref.Status == 507:
		if required {
			return true, ref
		}
		t.warn(ctx, ref.Error())
		if freeze {
			if after <= 0 {
				after = registryFullWait
			}
			return true, &errDeferred{after: after, reason: ref.Error()}
		}
		return true, nil // the exit snapshot is not retried: the last good one stays
	}
	return false, nil
}
