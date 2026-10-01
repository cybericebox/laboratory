package laboratory

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func retentionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeCatalog struct {
	repos   []string
	deleted []string
}

func (f *fakeCatalog) Repos(context.Context) ([]string, error) { return f.repos, nil }
func (f *fakeCatalog) DeleteRepo(_ context.Context, repo string) error {
	f.deleted = append(f.deleted, repo)
	return nil
}

func TestRetentionSweep(t *testing.T) {
	ctx := context.Background()
	s := retentionScheme(t)
	live := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "alive", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(live).Build()
	cat := &fakeCatalog{repos: []string{
		"lab/ns/alive/web", "lab/ns/gone/web", "lab/ns/gone/db", "base", "other/thing",
	}}
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sw := &RetentionSweeper{Client: c, Registry: cat, Retention: 72 * time.Hour, Namespace: "laboratory-system", Now: func() time.Time { return clock }}
	tombstones := func() map[string]string {
		var cm corev1.ConfigMap
		if err := c.Get(ctx, types.NamespacedName{Namespace: "laboratory-system", Name: RetentionConfigMap}, &cm); err != nil {
			return nil
		}
		return cm.Data
	}

	// The API server refuses ConfigMap keys with a slash: the keys must be valid ones.
	if err := sw.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for k := range tombstones() {
		if errs := validation.IsConfigMapKey(k); len(errs) != 0 {
			t.Fatalf("tombstone key %q is not a valid ConfigMap key: %v", k, errs)
		}
	}
	// First pass: the missing lab is only noted; a live lab is never touched.
	if err := sw.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cat.deleted) != 0 {
		t.Fatalf("nothing is deleted before the retention has passed: %v", cat.deleted)
	}
	if got := tombstones(); got["ns_gone"] != clock.Format(time.RFC3339) || len(got) != 1 {
		t.Fatalf("tombstones %v", got)
	}

	clock = clock.Add(71 * time.Hour)
	if err := sw.Sweep(ctx); err != nil || len(cat.deleted) != 0 {
		t.Fatalf("still within retention: %v %v", err, cat.deleted)
	}

	clock = clock.Add(2 * time.Hour)
	if err := sw.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cat.deleted) != 2 || cat.deleted[0] != "lab/ns/gone/web" && cat.deleted[0] != "lab/ns/gone/db" {
		t.Fatalf("both repositories of the gone lab must be deleted, got %v", cat.deleted)
	}
	for _, d := range cat.deleted {
		if d == "lab/ns/alive/web" || d == "base" || d == "other/thing" {
			t.Fatalf("deleted %s", d)
		}
	}

	// The registry keeps listing the empty repositories until its GC; they are not deleted again.
	n := len(cat.deleted)
	clock = clock.Add(200 * time.Hour)
	if err := sw.Sweep(ctx); err != nil || len(cat.deleted) != n {
		t.Fatalf("deleted repositories must not be deleted again: %v %v", err, cat.deleted)
	}

	// Once the registry forgets them, so does the sweep.
	cat.repos = []string{"lab/ns/alive/web"}
	if err := sw.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := tombstones(); len(got) != 0 {
		t.Fatalf("tombstones must be forgotten: %v", got)
	}
}

func TestRetentionLabThatReturnsCancelsDeletion(t *testing.T) {
	ctx := context.Background()
	s := retentionScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cat := &fakeCatalog{repos: []string{"lab/ns/back/web"}}
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sw := &RetentionSweeper{Client: c, Registry: cat, Retention: time.Hour, Namespace: "laboratory-system", Now: func() time.Time { return clock }}
	if err := sw.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "back", Namespace: "ns"}}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Hour)
	if err := sw.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cat.deleted) != 0 {
		t.Fatalf("a lab recreated under the same name keeps its snapshots: %v", cat.deleted)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "laboratory-system", Name: RetentionConfigMap}, &cm); err == nil && len(cm.Data) != 0 {
		t.Fatalf("tombstone must be cleared: %v", cm.Data)
	}
}
