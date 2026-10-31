package nodeagent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cybericebox/laboratory/internal/names"
)

func TestNodeLabelerMarksItsNodeAndClearsItOnStop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"keep": "me"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
	).Build()
	labels := func(name string) map[string]string {
		var n corev1.Node
		if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
			t.Fatal(err)
		}
		return n.Labels
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	l := &NodeLabeler{Client: c, NodeName: "n1", Interval: 10 * time.Millisecond}
	go func() { done <- l.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for labels("n1")[names.LabelNodeAgentReady] != "true" {
		if time.Now().After(deadline) {
			t.Fatal("the node was not marked ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if labels("n1")["keep"] != "me" {
		t.Errorf("other labels stay: %v", labels("n1"))
	}
	if _, ok := labels("n2")[names.LabelNodeAgentReady]; ok {
		t.Error("another node must not be marked")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := labels("n1")[names.LabelNodeAgentReady]; ok {
		t.Errorf("the label must go when the node-agent stops: %v", labels("n1"))
	}
	if labels("n1")["keep"] != "me" {
		t.Errorf("other labels stay: %v", labels("n1"))
	}
}

// The result of the start-up check is on the node in the same patch as the ready label, and a failed check is told once in an Event.
func TestNodeLabelerPublishesThePreflightWithTheLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}).Build()
	rec := record.NewFakeRecorder(10)
	pre := PreflightResult{StatePersistence: Unavailable("no containerd root"), RuntimeObservation: PreflightReady}
	l := &NodeLabeler{Client: c, NodeName: "n1", Interval: 10 * time.Millisecond, Annotations: pre.Annotations(), Recorder: rec}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	var n corev1.Node
	for {
		if err := c.Get(context.Background(), types.NamespacedName{Name: "n1"}, &n); err != nil {
			t.Fatal(err)
		}
		if n.Labels[names.LabelNodeAgentReady] == "true" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not marked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n.Annotations[names.AnnotationStatePersistence] != "unavailable: no containerd root" || n.Annotations[names.AnnotationRuntimeObservation] != "ready" {
		t.Fatalf("annotations %v", n.Annotations)
	}
	cancel()
	<-done
	if len(rec.Events) != 1 {
		t.Fatalf("one Event for the failed check, got %d", len(rec.Events))
	}
	if ev := <-rec.Events; !strings.Contains(ev, "LifecyclePreflightFailed") || !strings.Contains(ev, "no containerd root") {
		t.Errorf("event %q", ev)
	}
}
