package devicestate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// What the node does with a push the registry gate refused by a rule of its own (the limits live in the gate: it measures what it sees
// and does not trust what the node says about its own sizes). A refusal is not a failure of the device:
//
//   - 429 with Retry-After (the least interval between pushes, the push budget, the limits of exit snapshots and of open uploads): the snapshot is deferred for
//     the time the gate named, and taken again then; nothing is lost, the next snapshot has it;
//   - 507 CIB_REGISTRY_FULL: the last good snapshot stays; the lab status warning says why, and the live snapshot is tried again later;
//   - 403 CIB_TENANT_QUOTA: the tenant's snapshots would take more than its quota: the layer is not retried as it is, and the warning says so.
const (
	// defaultGateRetry is the wait of a 429 that named none, registryFullRetry the wait after a full registry.
	defaultGateRetry = time.Minute
	registryFullWait = 5 * time.Minute
	restartWait      = 5 * time.Second
	unavailableWait  = 30 * time.Second
	// maxGateRetry caps what a Retry-After may ask for (the gate's own hourly limits are the longest).
	maxGateRetry = time.Hour
)

// refusalDeferral is the deferral of an answer that asks to wait or to start again (429, 503 with Retry-After, a restart code), nil for any
// other refusal.
func refusalDeferral(ref *snapshot.Refusal) *errDeferred {
	after := ref.RetryAfter
	switch {
	case ref.Restart():
		// The gate has no live grant or session for it any more: start again from the first request, a moment later.
		return &errDeferred{after: clampRetry(restartWait), reason: ref.Error()}
	case ref.Status == http.StatusServiceUnavailable:
		// The gate's view of the cluster is not in contact: it says when to come back, and that is when it is tried again.
		if after <= 0 {
			after = unavailableWait
		}
		return &errDeferred{after: clampRetry(after), reason: ref.Error()}
	case ref.Status == http.StatusTooManyRequests:
		if after <= 0 {
			after = defaultGateRetry
		}
		return &errDeferred{after: clampRetry(after), reason: ref.Error()}
	}
	return nil
}

// minGateRetry is the least wait of a deferral: a Retry-After of a second or none at all must not turn into a loop of requests.
var minGateRetry = 3 * time.Second

// clampRetry keeps a wait between minGateRetry and maxGateRetry.
func clampRetry(d time.Duration) time.Duration {
	switch {
	case d < minGateRetry:
		return minGateRetry
	case d > maxGateRetry:
		return maxGateRetry
	}
	return d
}

// gateRefusal maps the error of a push onto the deferral and the lab status warning. handled says the error was a refusal of the gate and
// result is what the snapshot returns; otherwise the caller treats err as any failure of a push.
func (t *tracked) gateRefusal(ctx context.Context, err error, required, freeze bool, digest string) (handled bool, result error) {
	var ref *snapshot.Refusal
	if !errors.As(err, &ref) {
		return false, nil
	}
	after := ref.RetryAfter
	if d := refusalDeferral(ref); d != nil {
		return true, d
	}
	switch {
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

// readPackedLayer reads a packed layer of the original image through the relay. A 404 BLOB_UNKNOWN means the gate has no live grant for the blob
// (it is made by a request for the manifest): the manifest is asked for and the blob once more, as the kubelet does, and a pull that still
// does not go through is deferred, not failed; so is a 429 or 503 with Retry-After (the time the gate named).
func readPackedLayer(ctx context.Context, repo name.Repository, manifest v1.Hash, keychain authn.Keychain, base http.RoundTripper, h v1.Hash) (io.ReadCloser, error) {
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	ctx, note := snapshot.NewNote(ctx)
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain), remote.WithTransport(snapshot.Observe(base))}
	for attempt := 0; ; attempt++ {
		layer, err := remote.Layer(repo.Digest(h.String()), opts...)
		var rc io.ReadCloser
		if err == nil {
			rc, err = layer.Compressed() // remote verifies the requested digest at EOF
		}
		if err == nil {
			return rc, nil
		}
		ref, ok := snapshot.Classify(err, note).(*snapshot.Refusal)
		if !ok {
			return nil, fmt.Errorf("read original packed layer %s: %w", h, err)
		}
		if ref.Restart() && attempt == 0 && manifest != (v1.Hash{}) {
			if _, herr := remote.Head(repo.Digest(manifest.String()), opts...); herr == nil {
				continue
			}
		}
		if d := refusalDeferral(ref); d != nil {
			return nil, fmt.Errorf("read original packed layer %s: %w", h, d)
		}
		return nil, fmt.Errorf("read original packed layer %s: %w", h, ref)
	}
}
