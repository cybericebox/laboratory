package nodeagent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cybericebox/laboratory/internal/names"
)

// The node-agent's start-up check annotations carry no state-persistence value: node-state owns that annotation.
func TestNodeAgentDoesNotAssertTheStatePersistenceAnnotation(t *testing.T) {
	pre := PreflightResult{StatePersistence: PreflightDisabled, RuntimeObservation: PreflightReady}
	got := withoutStatePersistence(pre.Annotations())
	if _, has := got[names.AnnotationStatePersistence]; has {
		t.Fatalf("the node-agent must not put a value over node-state's annotation: %v", got)
	}
	if got[names.AnnotationRuntimeObservation] != PreflightReady || len(got) != 1 {
		t.Fatalf("the observation annotation stays: %v", got)
	}
}

func TestEnginePlan(t *testing.T) {
	failed := Unavailable(CodeSnapshotRegistry, "the registry is down")
	for _, tc := range []struct {
		name         string
		asked        bool
		result       string
		start, retry bool
	}{
		{"not asked", false, PreflightDisabled, false, false},
		{"not asked, whatever the result", false, failed, false, false},
		{"asked and ready", true, PreflightReady, true, false},
		{"asked and the check failed: retry, no engine yet", true, failed, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, retry := EnginePlan(tc.asked, tc.result)
			if start != tc.start || retry != tc.retry {
				t.Fatalf("EnginePlan(%v, %q) = %v, %v; want %v, %v", tc.asked, tc.result, start, retry, tc.start, tc.retry)
			}
		})
	}
}

func nodeAnnotatorFixture(t *testing.T) (*NodeAnnotator, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Annotations: map[string]string{"keep": "me"}, Labels: map[string]string{"l": "v"}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	rec := record.NewFakeRecorder(10)
	return &NodeAnnotator{Client: kube, NodeName: "n1", Recorder: rec}, kube, rec
}

// The annotator puts its annotations on the node and touches nothing else (no label, no foreign annotation).
func TestNodeAnnotatorSetsOnlyItsAnnotations(t *testing.T) {
	a, kube, _ := nodeAnnotatorFixture(t)
	if err := a.SetAnnotation(context.Background(), names.AnnotationStatePersistence, PreflightReady); err != nil {
		t.Fatal(err)
	}
	var n corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: "n1"}, &n); err != nil {
		t.Fatal(err)
	}
	if n.Annotations[names.AnnotationStatePersistence] != PreflightReady || n.Annotations["keep"] != "me" {
		t.Fatalf("annotations %v", n.Annotations)
	}
	if _, set := n.Labels[names.LabelNodeAgentReady]; set || n.Labels["l"] != "v" {
		t.Fatalf("the annotator must not touch labels: %v", n.Labels)
	}
}

// A later value replaces the first and is what is asserted again; a failed check is told once as a Warning Event.
func TestNodeAnnotatorKeepsTheLatestValueAndTellsAFailureOnce(t *testing.T) {
	a, kube, rec := nodeAnnotatorFixture(t)
	failed := Unavailable(CodeSnapshotRegistry, "the registry is down")
	for i := 0; i < 2; i++ {
		if err := a.SetAnnotation(context.Background(), names.AnnotationStatePersistence, failed); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.Events) != 1 {
		t.Fatalf("the same failure is told once, got %d events", len(rec.Events))
	}
	if ev := <-rec.Events; !strings.Contains(ev, "LifecyclePreflightFailed") || !strings.Contains(ev, "the registry is down") {
		t.Fatalf("event %q", ev)
	}
	if err := a.SetAnnotation(context.Background(), names.AnnotationStatePersistence, PreflightReady); err != nil {
		t.Fatal(err)
	}
	// someone removes it: the next assertion puts the latest value back
	var n corev1.Node
	_ = kube.Get(context.Background(), client.ObjectKey{Name: "n1"}, &n)
	delete(n.Annotations, names.AnnotationStatePersistence)
	if err := kube.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	if err := a.patch(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = kube.Get(context.Background(), client.ObjectKey{Name: "n1"}, &n)
	if n.Annotations[names.AnnotationStatePersistence] != PreflightReady {
		t.Fatalf("annotations %v", n.Annotations)
	}
}
