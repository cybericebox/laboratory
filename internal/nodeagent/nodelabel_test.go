package nodeagent

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
