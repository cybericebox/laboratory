package checker

import (
	"context"

	"github.com/cybericebox/laboratory/internal/probe"
)

// Probe asks the in-device program one built-in observation (probe check-observe). The answer carries facts only; the comparison with
// the expected values is the checker's.
func (o *execObserver) Probe(ctx context.Context, req probe.ObserveRequest) (probe.ObserveResponse, error) {
	var resp probe.ObserveResponse
	if err := o.ask(ctx, probe.CommandCheckObserve, req, &resp); err != nil {
		return probe.ObserveResponse{}, err
	}
	return resp, nil
}

// ProbeFile reads a file through the in-device program (probe check-file).
func (o *execObserver) ProbeFile(ctx context.Context, path string, wantContent bool) (FileFacts, error) {
	return o.ReadFile(ctx, path, wantContent)
}
