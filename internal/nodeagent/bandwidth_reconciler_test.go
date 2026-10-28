package nodeagent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"
)

type limiterSpy struct{ got []bandwidth.Limits }

func (l *limiterSpy) SetGeneveLimits(x bandwidth.Limits) error { l.got = append(l.got, x); return nil }

func podOn(name, node, reserve string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Annotations: map[string]string{names.AnnotationBandwidthReserve: reserve}},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func TestReconcileTurnsTheLabelAndTheReservesIntoGeneveLimits(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{names.LabelNodeBandwidth: "1562"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node, podOn("registry", "n1", "100"), podOn("elsewhere", "n2", "500")).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string { return []string{o.(*corev1.Pod).Spec.NodeName} }).Build()
	spy := &limiterSpy{}
	r := &BandwidthReconciler{Client: c, NodeName: "n1", Config: bandwidth.DefaultConfig(), Flows: spy}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	// U = 1562 - 100 system - 100 registry = 1362; the pod of node n2 is not subtracted
	want := bandwidth.DefaultConfig().Compute(1562, []int64{100}).GeneveKbps
	if len(spy.got) != 1 || spy.got[0].GeneveKbps != want || spy.got[0].LabCapKbps != 100_000 {
		t.Fatalf("limits %+v, want geneve %d", spy.got, want)
	}
}

func TestReconcileOfAMissingNodeDoesNothing(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	spy := &limiterSpy{}
	r := &BandwidthReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), NodeName: "gone", Config: bandwidth.DefaultConfig(), Flows: spy}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil || len(spy.got) != 0 {
		t.Fatalf("%v %v", err, spy.got)
	}
}
