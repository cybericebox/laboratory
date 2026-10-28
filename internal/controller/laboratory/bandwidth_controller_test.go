package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"
)

func bwNode(name, mbit string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if mbit != "" {
		n.Labels = map[string]string{names.LabelNodeBandwidth: mbit}
	}
	return n
}

func bwPod(ns, name, node string, labels map[string]string, reserve string) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}, Spec: corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if reserve != "" {
		p.Annotations = map[string]string{names.AnnotationBandwidthReserve: reserve}
	}
	return p
}

var (
	gatewayLabels = map[string]string{names.LabelComponent: names.ComponentGateway}
	demuxLabels   = map[string]string{"app": names.WGDemuxApp}
	l7Labels      = map[string]string{"app": names.ProxyL7App}
)

func limitsOf(t *testing.T, plan map[types.NamespacedName]string, ns, name string) bandwidth.Limits {
	t.Helper()
	v, ok := plan[types.NamespacedName{Namespace: ns, Name: name}]
	if !ok {
		t.Fatalf("no ceiling planned for %s/%s: %v", ns, name, plan)
	}
	l, err := bandwidth.Decode(v)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPlanDividesTheNodeBetweenEntryAndInternet(t *testing.T) {
	cfg := bandwidth.DefaultConfig()
	nodes := []corev1.Node{bwNode("n1", "1562")}
	pods := []corev1.Pod{
		bwPod("laboratory-wg", "demux", "n1", demuxLabels, ""),
		bwPod("laboratory-proxy", "l7", "n1", l7Labels, ""),
		bwPod("lg-a", "gateway-a", "n1", gatewayLabels, ""),
		bwPod("lg-b", "gateway-b", "n1", gatewayLabels, ""),
		bwPod("laboratory-system", "registry", "n1", nil, "100"),
	}
	// 5 labs in group a, 5 in group b: ten labs share the internet class of the node
	plan := bandwidthPlan(cfg, nodes, pods, map[string]int{"lg-a": 5, "lg-b": 5})
	// U = 1562 - 100 system - 100 registry = 1362 Mbit/s; entry 681, halved between the two entry pods
	for _, name := range [][2]string{{"laboratory-wg", "demux"}, {"laboratory-proxy", "l7"}} {
		l := limitsOf(t, plan, name[0], name[1])
		if l.ClassKbps != 340_500 || l.CapKbps != 50_000 {
			t.Errorf("%v: %+v", name, l)
		}
	}
	// internet 25% of 1362 = 340.5 Mbit/s over ten labs = 34 Mbit/s each, under the 50 Mbit/s cap
	for _, gw := range []string{"gateway-a", "gateway-b"} {
		if l := limitsOf(t, plan, map[string]string{"gateway-a": "lg-a", "gateway-b": "lg-b"}[gw], gw); l.CapKbps != 34_050 || l.ClassKbps != 0 {
			t.Errorf("%s: %+v", gw, l)
		}
	}
	if _, ok := plan[types.NamespacedName{Namespace: "laboratory-system", Name: "registry"}]; ok {
		t.Error("a reserve pod gets no ceiling, it only gives up bandwidth")
	}
}

func TestPlanCapsALabAloneOnTheNode(t *testing.T) {
	plan := bandwidthPlan(bandwidth.DefaultConfig(), []corev1.Node{bwNode("n1", "")}, []corev1.Pod{bwPod("lg-a", "gw", "n1", gatewayLabels, "")}, map[string]int{"lg-a": 1})
	if l := limitsOf(t, plan, "lg-a", "gw"); l.CapKbps != 50_000 {
		t.Fatalf("one lab gets the lab cap, not the whole class: %+v", l)
	}
}

func TestPlanCountsLabsPerNodeAndIgnoresPodsOfOtherNodes(t *testing.T) {
	cfg := bandwidth.DefaultConfig()
	nodes := []corev1.Node{bwNode("n1", "1562"), bwNode("n2", "1562")}
	pods := []corev1.Pod{
		bwPod("lg-a", "gw", "n1", gatewayLabels, ""),
		bwPod("lg-b", "gw", "n2", gatewayLabels, ""),
		bwPod("platform", "backend", "n2", nil, "500"), // only n2 gives this up
	}
	plan := bandwidthPlan(cfg, nodes, pods, map[string]int{"lg-a": 20, "lg-b": 20})
	a, b := limitsOf(t, plan, "lg-a", "gw"), limitsOf(t, plan, "lg-b", "gw")
	// n1: 1462*0.25 = 365.5 Mbit/s / 20 = 18275; n2: 962*0.25 = 240.5 / 20 = 12025
	if a.CapKbps != 18_275 || b.CapKbps != 12_025 {
		t.Fatalf("n1 %d n2 %d", a.CapKbps, b.CapKbps)
	}
}

func TestPlanSkipsPodsThatAreNotPlacedOrAreOver(t *testing.T) {
	done := bwPod("lg-a", "old", "n1", gatewayLabels, "")
	done.Status.Phase = corev1.PodSucceeded
	pending := bwPod("lg-a", "pending", "", gatewayLabels, "")
	plan := bandwidthPlan(bandwidth.DefaultConfig(), []corev1.Node{bwNode("n1", "")}, []corev1.Pod{done, pending}, map[string]int{"lg-a": 3})
	if len(plan) != 0 {
		t.Fatalf("a pod without a node, or finished, gets nothing: %v", plan)
	}
}

func TestPlanRecognizesTheOldGatewayLabel(t *testing.T) {
	old := bwPod("lg-a", "gw", "n1", map[string]string{"app": names.ComponentGateway}, "")
	plan := bandwidthPlan(bandwidth.DefaultConfig(), []corev1.Node{bwNode("n1", "")}, []corev1.Pod{old}, map[string]int{"lg-a": 1})
	if len(plan) != 1 {
		t.Fatalf("a gateway made before the component label is still a gateway: %v", plan)
	}
}

func TestReconcileWritesOnlyWhatDiffers(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	node := bwNode("n1", "1562")
	gw := bwPod("lg-a", "gw", "n1", gatewayLabels, "")
	lab := &laboratoryv1alpha1.LabGateway{ObjectMeta: metav1.ObjectMeta{Namespace: "lg-a", Name: "g1"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(&node, &gw, lab).Build()
	r := &BandwidthReconciler{Client: c, Config: bandwidth.DefaultConfig()}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lg-a", Name: "gw"}, &got); err != nil {
		t.Fatal(err)
	}
	l, err := bandwidth.Decode(got.Annotations[names.AnnotationBandwidthLimits])
	if err != nil || l.CapKbps != 50_000 {
		t.Fatalf("annotation %q -> %+v %v", got.Annotations[names.AnnotationBandwidthLimits], l, err)
	}
	rv := got.ResourceVersion
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "lg-a", Name: "gw"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != rv {
		t.Fatal("a second pass with nothing changed must write nothing")
	}
}
