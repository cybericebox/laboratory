package crdcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/cybericebox/laboratory/internal/nodecap"
)

// chartCRDs serves the chart's Device and LabGroup CRDs; drop removes a field of the gang commit from both.
func chartCRDs(t *testing.T, drop string) GetCRD {
	t.Helper()
	crds := map[string]map[string]any{}
	for name, f := range map[string]string{"devices." + group: "laboratory.cybericebox.com_devices.yaml", "labgroups." + group: "laboratory.cybericebox.com_labgroups.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "laboratory", "crds", f))
		if err != nil {
			t.Fatal(err)
		}
		if drop != "" {
			// an older CRD: the field is not in the schema
			raw = regexp.MustCompile(`(?m)^(\s+)`+drop+`:`).ReplaceAll(raw, []byte("${1}x-old-"+drop+":"))
		}
		var m map[string]any
		if err := yaml.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		crds[name] = m
	}
	return func(_ context.Context, name string) (map[string]any, error) {
		if m, ok := crds[name]; ok {
			return m, nil
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, name)
	}
}

func TestGangCommitFieldsMissing(t *testing.T) {
	if missing, err := GangCommitFieldsMissing(context.Background(), chartCRDs(t, "")); err != nil || len(missing) != 0 {
		t.Fatalf("current CRDs: %v %v", missing, err)
	}
	for _, f := range GangCommitFields {
		missing, err := GangCommitFieldsMissing(context.Background(), chartCRDs(t, f))
		if err != nil || len(missing) != 2 || !strings.HasSuffix(missing[1], "labgroups.laboratory.cybericebox.com: "+f) {
			t.Fatalf("without %s: %v %v", f, missing, err)
		}
	}
}

var budget = nodecap.Maximum{Budget: nodecap.Amount{CPU: 9885, Memory: 44757612 << 10}}

// The budget is used only with current CRDs; a missing field, a missing CRD or a refused read turns it off at once; a read that
// fails for a passing reason is retried, and the budget stays when it never gets an answer.
func TestUsableBudget(t *testing.T) {
	ctx := context.Background()
	if got, why, kept := UsableBudget(ctx, chartCRDs(t, ""), budget, time.Second); got != budget || why != "" || kept != nil {
		t.Fatalf("current CRDs: %+v %q", got, why)
	}
	if got, why, _ := UsableBudget(ctx, chartCRDs(t, "onBudget"), budget, time.Second); got.Set() || !strings.Contains(why, "onBudget") {
		t.Fatalf("an older CRD: %+v %q", got, why)
	}
	gr := schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}
	for name, err := range map[string]error{"missing": apierrors.NewNotFound(gr, "devices."+group), "forbidden": apierrors.NewForbidden(gr, "devices."+group, errors.New("no"))} {
		calls := 0
		get := func(context.Context, string) (map[string]any, error) { calls++; return nil, err }
		if got, why, _ := UsableBudget(ctx, get, budget, time.Minute); got.Set() || why == "" || calls != 1 {
			t.Fatalf("%s: %+v %q after %d reads", name, got, why, calls)
		}
	}
	current, calls := chartCRDs(t, ""), 0
	flaky := func(ctx context.Context, name string) (map[string]any, error) {
		calls++
		if calls < 3 {
			return nil, apierrors.NewServerTimeout(gr, "get", 1)
		}
		return current(ctx, name)
	}
	if got, _, kept := UsableBudget(ctx, flaky, budget, time.Minute); got != budget || calls < 3 || kept != nil {
		t.Fatalf("a passing error is retried: %+v after %d reads", got, calls)
	}
	down := func(context.Context, string) (map[string]any, error) {
		return nil, apierrors.NewServerTimeout(gr, "get", 1)
	}
	if got, why, kept := UsableBudget(ctx, down, budget, 1500*time.Millisecond); got != budget || why != "" || !apierrors.IsServerTimeout(kept) {
		t.Fatalf("no answer keeps the budget and says why: %+v %q %v", got, why, kept)
	}
	if got, why, _ := UsableBudget(ctx, down, nodecap.Maximum{}, time.Minute); got.Set() || why != "" {
		t.Fatal("no budget: nothing to check")
	}
}
