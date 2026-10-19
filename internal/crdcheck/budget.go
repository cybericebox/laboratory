package crdcheck

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/cybericebox/laboratory/internal/nodecap"
)

// The capacity budget relies on the scheduling fields of the gang commit: an API server serving an older Device or LabGroup
// CRD prunes them, and a withdrawal then deletes nothing and holds nothing back. The operator and the agent both read the two
// CRDs (get on them by name) before they use a budget, so they never disagree on whether it is on.

// GetCRD returns the CustomResourceDefinition of that name as a JSON object; a missing CRD or a refused read is the API's
// NotFound or Forbidden error.
type GetCRD func(ctx context.Context, name string) (map[string]any, error)

const (
	group   = "laboratory.cybericebox.com"
	version = "v1alpha1"
	// props is the key of an object's fields in an OpenAPI schema.
	props = "properties"
)

// GangCommitFields are the scheduling fields the gang commit writes.
var GangCommitFields = []string{"withdrawals", "retryAfter", "onBudget", "schedulerMessage"}

// GangCommitFieldsMissing names the fields of the gang commit the served Device and LabGroup CRDs lack ("<crd>: <field>");
// none: the CRDs are current. An error is the read's.
func GangCommitFieldsMissing(ctx context.Context, get GetCRD) ([]string, error) {
	var missing []string
	for _, c := range []struct {
		name string
		path []string // from the status schema to the PodSchedule properties
	}{
		{"devices." + group, []string{props, "scheduling", props}},
		{"labgroups." + group, []string{props, "pods", "items", props}},
	} {
		crd, err := get(ctx, c.name)
		if err != nil {
			return nil, fmt.Errorf("read the CRD %s: %w", c.name, err)
		}
		fields := servedStatusSchema(crd)
		for _, k := range c.path {
			next, _ := fields[k].(map[string]any)
			fields = next
		}
		for _, f := range GangCommitFields {
			if _, ok := fields[f]; !ok {
				missing = append(missing, c.name+": "+f)
			}
		}
	}
	return missing, nil
}

// servedStatusSchema is the status schema of the laboratory API version in the CRD; nil when it has none.
func servedStatusSchema(crd map[string]any) map[string]any {
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok || m["name"] != version {
			continue
		}
		out := m
		for _, k := range []string{"schema", "openAPIV3Schema", props, "status"} {
			out, _ = out[k].(map[string]any)
		}
		return out
	}
	return nil
}

// BudgetRetry is how long UsableBudget retries a read that fails for a passing reason.
const BudgetRetry = 30 * time.Second

// UsableBudget is the capacity maximum to run with: m when it has no budget or the served CRDs know the fields of the gang
// commit; none (the maximum is the current capacity) when a CRD lacks them, is missing or may not be read, with why. A read that
// fails for another reason (the API server busy, a timeout) is retried with a growing pause for up to retry; if it still fails
// the budget is kept, since nothing says the CRDs are old.
func UsableBudget(ctx context.Context, get GetCRD, m nodecap.Maximum, retry time.Duration) (nodecap.Maximum, string) {
	if !m.Set() {
		return m, ""
	}
	deadline := time.Now().Add(retry)
	pause := time.Second
	for {
		missing, err := GangCommitFieldsMissing(ctx, get)
		switch {
		case err == nil && len(missing) == 0:
			return m, ""
		case err == nil:
			return nodecap.Maximum{}, fmt.Sprintf("the served CRDs lack %v", missing)
		case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
			return nodecap.Maximum{}, err.Error()
		case !time.Now().Add(pause).Before(deadline):
			return m, ""
		}
		select {
		case <-ctx.Done():
			return m, ""
		case <-time.After(pause):
		}
		pause = min(2*pause, 8*time.Second)
	}
}
