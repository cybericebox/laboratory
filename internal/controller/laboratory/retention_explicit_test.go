package laboratory

import (
	"context"
	"encoding/json"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

type partialRetirementCatalog struct {
	fakeCatalog
	fail bool
}

func (c *partialRetirementCatalog) DeleteRepo(ctx context.Context, repo string) error {
	if c.fail && repo == "lab/ns/child/db" {
		return fmt.Errorf("registry unavailable")
	}
	return c.fakeCatalog.DeleteRepo(ctx, repo)
}
func TestRetentionExplicitStoppedDeadlineAndPartialRetry(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	child := groupStoppedChild()
	deadline := metav1.NewTime(clock.Add(time.Hour))
	child.Spec.Lifecycle.RetentionUntil = &deadline
	child.Spec.Lifecycle.Terminal = true
	c := fake.NewClientBuilder().WithScheme(retentionScheme(t)).WithStatusSubresource(child).WithObjects(child).Build()
	catalog := &partialRetirementCatalog{fakeCatalog: fakeCatalog{repos: []string{"lab/ns/child/web", "lab/ns/child/db"}}, fail: true}
	sweeper := &RetentionSweeper{Client: c, Reader: c, Registry: catalog, Namespace: "operator", Now: func() time.Time { return clock }}
	if err := sweeper.Sweep(ctx); err != nil || len(catalog.deleted) != 0 {
		t.Fatalf("deleted before deadline %v %v", catalog.deleted, err)
	}
	clock = clock.Add(time.Hour)
	if err := sweeper.Sweep(ctx); err == nil {
		t.Fatal("partial registry failure not reported")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	f, ok := lab.ParseSnapshotRetirement(child.Annotations[names.AnnotationSnapshotRetirement])
	if !ok || f.State != "CleanupPending" {
		t.Fatalf("pending fence %+v", f)
	}
	catalog.fail = false
	if err := sweeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	f, ok = lab.ParseSnapshotRetirement(child.Annotations[names.AnnotationSnapshotRetirement])
	if !ok || f.State != "Deleted" || !child.Spec.Lifecycle.Terminal {
		t.Fatalf("retired definition missing or resurrected: %+v", child)
	}
	count := len(catalog.deleted)
	if err := sweeper.Sweep(ctx); err != nil || len(catalog.deleted) != count {
		t.Fatal("completed retirement repeated")
	}
}
func TestRetentionNewerNonterminalStartFencesExplicitExpiry(t *testing.T) {
	ctx := context.Background()
	child := groupStoppedChild()
	now := time.Now()
	deadline := metav1.NewTime(now.Add(-time.Hour))
	child.Spec.Lifecycle.RetentionUntil = &deadline
	child.Spec.Lifecycle.DesiredState = "Running"
	child.Spec.Lifecycle.Revision++
	c := fake.NewClientBuilder().WithScheme(retentionScheme(t)).WithObjects(child).Build()
	catalog := &fakeCatalog{repos: []string{"lab/ns/child/web"}}
	sweeper := &RetentionSweeper{Client: c, Reader: c, Registry: catalog, Namespace: "operator", Now: func() time.Time { return now }}
	if err := sweeper.Sweep(ctx); err != nil || len(catalog.deleted) != 0 {
		t.Fatalf("expired prior stop deleted new start %v %v", catalog.deleted, err)
	}
}

func TestRetentionManagedBirthRequiresExplicitLifecycleRetirement(t *testing.T) {
	ctx := context.Background()
	child := groupStoppedChild()
	now := time.Now()
	deadline := metav1.NewTime(now.Add(-time.Hour))
	child.Spec.Lifecycle.RetentionUntil = &deadline
	child.Spec.Lifecycle.Terminal = true
	birth := lab.LabCreationReceipt{LabName: child.Name, GroupUID: "group-uid", NamespaceUID: "namespace-uid", OperationID: "birth-op", Revision: 1, DefinitionHash: "definition", CreationID: "creation", LabUID: string(child.UID), Committed: true}
	raw, err := json.Marshal(birth)
	if err != nil {
		t.Fatal(err)
	}
	child.Annotations = map[string]string{names.AnnotationLabCreation: string(raw)}
	c := fake.NewClientBuilder().WithScheme(retentionScheme(t)).WithObjects(child).Build()
	catalog := &fakeCatalog{repos: []string{"lab/ns/child/web"}}
	sweeper := &RetentionSweeper{Client: c, Reader: c, Registry: catalog, Namespace: "operator", Now: func() time.Time { return now }}
	if err := sweeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(catalog.deleted) != 0 {
		t.Fatal("legacy expiry deleted platform-managed state without its explicit retirement", catalog.deleted)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
		t.Fatal(err)
	}
	if child.Annotations[names.AnnotationSnapshotRetirement] != "" || !child.Spec.Lifecycle.Terminal {
		t.Fatal("legacy expiry changed managed retirement authority or terminal state")
	}
}

func TestRetentionInvalidManagedBirthDoesNotFallBackToLegacyDeletion(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed":   `{`,
		"replacement": `{"labName":"child","groupUID":"group","namespaceUID":"ns","operationId":"birth","revision":1,"definitionHash":"hash","creationId":"creation","labUID":"old-child-uid","committed":true}`,
		"pending":     `{"labName":"child","groupUID":"group","namespaceUID":"ns","operationId":"birth","revision":1,"definitionHash":"hash","creationId":"creation","labUID":"child-uid","committed":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			child := groupStoppedChild()
			now := time.Now()
			deadline := metav1.NewTime(now.Add(-time.Hour))
			child.Spec.Lifecycle.RetentionUntil = &deadline
			child.Annotations = map[string]string{names.AnnotationLabCreation: raw}
			c := fake.NewClientBuilder().WithScheme(retentionScheme(t)).WithObjects(child).Build()
			catalog := &fakeCatalog{repos: []string{"lab/ns/child/web"}}
			sweeper := &RetentionSweeper{Client: c, Reader: c, Registry: catalog, Namespace: "operator", Now: func() time.Time { return now }}
			if err := sweeper.Sweep(ctx); err == nil {
				t.Fatal("uncertain managed identity was hidden")
			}
			if len(catalog.deleted) != 0 {
				t.Fatal("uncertain managed identity fell back to destructive legacy expiry")
			}
		})
	}
}
