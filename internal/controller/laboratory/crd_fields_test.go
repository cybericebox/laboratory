package laboratory

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/cybericebox/laboratory/internal/nodecap"
)

// crdFixture is a client serving the chart's Device and LabGroup CRDs; drop removes a field of the gang commit from both.
func crdFixture(t *testing.T, drop string) client.Client {
	t.Helper()
	var objs []client.Object
	for _, f := range []string{"laboratory.cybericebox.com_devices.yaml", "laboratory.cybericebox.com_labgroups.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "laboratory", "crds", f))
		if err != nil {
			t.Fatal(err)
		}
		if drop != "" {
			// an older CRD: the field is not in the schema
			raw = regexp.MustCompile(`(?m)^(\s+)`+drop+`:`).ReplaceAll(raw, []byte("${1}x-old-"+drop+":"))
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &u.Object); err != nil {
			t.Fatal(err)
		}
		objs = append(objs, u)
	}
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objs...).Build()
}

// With a budget and an older CRD the operator runs without the budget and says why; with current CRDs, or no budget, it runs
// as configured.
func TestBudgetWithCurrentCRDs(t *testing.T) {
	budget := nodecap.Maximum{Budget: nodecap.Amount{CPU: 9885, Memory: 44757612 << 10}}
	rec := record.NewFakeRecorder(4)
	if got := BudgetWithCurrentCRDs(context.Background(), crdFixture(t, ""), rec, budget); got != budget || len(rec.Events) != 0 {
		t.Fatalf("current CRDs: %+v, %d events", got, len(rec.Events))
	}
	if got := BudgetWithCurrentCRDs(context.Background(), crdFixture(t, "schedulerMessage"), rec, budget); got.Set() {
		t.Fatalf("an older CRD keeps the budget: %+v", got)
	}
	if e := <-rec.Events; !strings.Contains(e, "CapacityBudgetDisabled") || !strings.Contains(e, "schedulerMessage") {
		t.Fatalf("event = %q", e)
	}
	if got := BudgetWithCurrentCRDs(context.Background(), crdFixture(t, "schedulerMessage"), rec, nodecap.Maximum{}); got.Set() || len(rec.Events) != 0 {
		t.Fatal("no budget: nothing to check")
	}
}
