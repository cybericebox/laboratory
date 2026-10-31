package grpc

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func readyNode(name string, persistence, observation string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{names.LabelNodeAgentReady: "true"}, Annotations: map[string]string{}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if persistence != "" {
		n.Annotations[names.AnnotationStatePersistence] = persistence
	}
	if observation != "" {
		n.Annotations[names.AnnotationRuntimeObservation] = observation
	}
	return n
}

func fullFeatures() *protobuf.FeaturesResponse {
	return &protobuf.FeaturesResponse{
		StatePersistence: &protobuf.StatePersistenceFeature{Available: true},
		Lifecycle:        &protobuf.LifecycleFeature{PerLabStop: true, ConfirmedRuntime: true, RetainedRestart: true, FullGroupStop: true},
	}
}

func TestVerifyNodesConfirmsWhatEveryReadyNodeConfirms(t *testing.T) {
	ok := nodeagent.PreflightReady
	nodes := []corev1.Node{readyNode("a", ok, ok), readyNode("b", ok, ok)}
	notReady := readyNode("c", nodeagent.Unavailable(nodeagent.CodeRuntimeSocket, "broken"), nodeagent.Unavailable(nodeagent.CodeRuntimeSocket, "broken"))
	delete(notReady.Labels, names.LabelNodeAgentReady) // a node whose node-agent is not serving is not asked
	v := verifyNodes(append(nodes, notReady), nil, nil, true, true)
	if v.checked != 2 || v.persistenceReason != "" || v.observationReason != "" {
		t.Fatalf("%+v", v)
	}
	f := fullFeatures()
	applyNodeSupport(f, v)
	if !f.StatePersistence.Available || !f.Lifecycle.RetainedRestart || f.Lifecycle.UnavailableReason != "" {
		t.Fatalf("a confirmed cluster keeps its features: %v %v", f.StatePersistence, f.Lifecycle)
	}
}

func TestPersistenceFailureHoldsBackPersistenceAndRetainedRestartWithTheReason(t *testing.T) {
	ok := nodeagent.PreflightReady
	nodes := []corev1.Node{readyNode("a", ok, ok), readyNode("b", nodeagent.Unavailable(nodeagent.CodeContainerdRootMissing, "the containerd root /secret/path does not exist on this node"), ok)}
	v := verifyNodes(nodes, nil, nil, true, true)
	f := fullFeatures()
	applyNodeSupport(f, v)
	if f.StatePersistence.Available || f.StatePersistence.UnavailableReason != "CONTAINERD_ROOT_MISSING:1/2" {
		t.Fatalf("persistence: %v", f.StatePersistence)
	}
	l := f.Lifecycle
	if l.RetainedRestart || !l.PerLabStop || !l.ConfirmedRuntime || !l.FullGroupStop {
		t.Fatalf("only retained restart needs the snapshots: %v", l)
	}
	if l.UnavailableReason != "CONTAINERD_ROOT_MISSING:1/2" {
		t.Errorf("reason: %q", l.UnavailableReason)
	}
}

func TestObservationFailureHoldsBackEveryLifecycleCapability(t *testing.T) {
	ok := nodeagent.PreflightReady
	nodes := []corev1.Node{readyNode("a", ok, nodeagent.Unavailable(nodeagent.CodeCgroupV2Missing, "no cgroup tree at /host/sys/fs/cgroup")), readyNode("b", ok, nodeagent.Unavailable(nodeagent.CodeCgroupV2Missing, "no cgroup tree at /host/sys/fs/cgroup"))}
	f := fullFeatures()
	applyNodeSupport(f, verifyNodes(nodes, nil, nil, true, true))
	l := f.Lifecycle
	if l.PerLabStop || l.ConfirmedRuntime || l.RetainedRestart || l.FullGroupStop || l.UnavailableReason != "CGROUP_V2_MISSING:2/2" {
		t.Fatalf("%v", l)
	}
	if !f.StatePersistence.Available {
		t.Error("persistence itself is unaffected by the observation")
	}
}

func TestUnreportedAndDisabledNodesHoldFeaturesBack(t *testing.T) {
	ok := nodeagent.PreflightReady
	for name, node := range map[string]corev1.Node{
		"no annotation (older node-agent)": readyNode("a", "", ok),
		"disabled on the node":             readyNode("a", nodeagent.PreflightDisabled, ok),
	} {
		f := fullFeatures()
		applyNodeSupport(f, verifyNodes([]corev1.Node{node}, nil, nil, true, true))
		if f.StatePersistence.Available || f.StatePersistence.UnavailableReason == "" {
			t.Errorf("%s: %v", name, f.StatePersistence)
		}
		if got := f.StatePersistence.UnavailableReason; got != "NODE_UNCONFIRMED:1/1" && got != "PERSISTENCE_DISABLED:1/1" {
			t.Errorf("%s: a fixed code, got %q", name, got)
		}
	}
}

// Nothing to ask (no ready node) or nothing configured: the configured values stand.
func TestNothingToVerifyKeepsTheConfiguredFeatures(t *testing.T) {
	f := fullFeatures()
	applyNodeSupport(f, verifyNodes(nil, nil, nil, true, true))
	if !f.StatePersistence.Available || !f.Lifecycle.PerLabStop {
		t.Fatalf("%v %v", f.StatePersistence, f.Lifecycle)
	}
	ok := nodeagent.PreflightReady
	v := verifyNodes([]corev1.Node{readyNode("a", nodeagent.Unavailable(nodeagent.CodeRuntimeSocket, "x"), nodeagent.Unavailable(nodeagent.CodeRuntimeSocket, "y"))}, nil, nil, false, false)
	if v.persistenceReason != "" || v.observationReason != "" {
		t.Errorf("a feature the chart does not configure is not judged: %+v", v)
	}
	_ = ok
}

// Through GetFeatures with real Nodes: the answer follows the nodes' result, not the chart's values.
func TestGetFeaturesFollowsTheNodes(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	h.features.StatePersistence = true
	h.features.Lifecycle = &protobuf.LifecycleFeature{PerLabStop: true, ConfirmedRuntime: true, RetainedRestart: true, FullGroupStop: true}
	h.statePersistence = true
	setAnn := func(name string, ann map[string]string) {
		n, err := k.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		n.Annotations = ann
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[names.LabelNodeAgentReady] = "true"
		if _, err := k.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	addNode(t, k, "w1", map[string]string{})
	setAnn("w1", map[string]string{names.AnnotationStatePersistence: nodeagent.Unavailable(nodeagent.CodeContainerdRootMissing, "the containerd root /var/lib/k0s/containerd does not exist on this node"), names.AnnotationRuntimeObservation: nodeagent.PreflightReady})
	f, err := h.GetFeatures(ctx, &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if f.GetStatePersistence().GetAvailable() || f.GetStatePersistence().GetUnavailableReason() != "CONTAINERD_ROOT_MISSING:1/1" {
		t.Fatalf("persistence: %v", f.GetStatePersistence())
	}
	// nothing of the cluster's inside leaves it: no node name, no path, no error text, in any field of the answer
	for _, leak := range []string{"w1", "/var/lib", "k0s", "does not exist on this node", "containerd root /"} {
		if strings.Contains(f.String(), leak) {
			t.Errorf("GetFeatures leaks %q: %s", leak, f.String())
		}
	}
	if f.GetLifecycle().GetRetainedRestart() || !f.GetLifecycle().GetPerLabStop() {
		t.Fatalf("lifecycle: %v", f.GetLifecycle())
	}
}

// With several failing nodes the code most of them give is reported with the count of failing nodes out of the nodes asked; the reason
// is always one of the fixed codes.
func TestReasonIsAFixedCodeWithACount(t *testing.T) {
	ok := nodeagent.PreflightReady
	nodes := []corev1.Node{
		readyNode("n1", nodeagent.Unavailable(nodeagent.CodeSnapshotRegistry, "http://registry.internal:5000 refused"), ok),
		readyNode("n2", nodeagent.Unavailable(nodeagent.CodeSnapshotRegistry, "x"), ok),
		readyNode("n3", nodeagent.Unavailable(nodeagent.CodeContainerdRootMissing, "y"), ok),
		readyNode("n4", ok, ok),
	}
	v := verifyNodes(nodes, nil, nil, true, true)
	if v.persistenceReason != "SNAPSHOT_REGISTRY_UNREACHABLE:3/4" {
		t.Fatalf("%q", v.persistenceReason)
	}
	for _, c := range nodeagent.AllCodes {
		if strings.ToUpper(c) != c || strings.ContainsAny(c, " :/") {
			t.Errorf("code %q must be upper case with no separators", c)
		}
	}
}
