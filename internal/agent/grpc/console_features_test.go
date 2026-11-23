package grpc

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent"
	"github.com/cybericebox/laboratory/internal/switches"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// GetFeatures reports the console cables (contract 3.1) and the kernel log of a console port (contract 3.2): the chart switch AND the relay of the nodes, the limits, and the port constants
// of a device. With the switch on, the feature is enabled only when every lab node ready for pods (and at least one) says its relay is
// ready (names.AnnotationConsoleRelay, written by its node-agent once the device access service of the node answers on cable.sock).
func TestFeaturesReportConsoleCables(t *testing.T) {
	ready := nodeagent.PreflightReady
	for name, c := range map[string]struct {
		on    bool
		nodes map[string]string // node -> annotation ("-": none)
		want  bool
		// old: nodes whose relay does not report the kernel log (names.AnnotationConsoleKernelLog absent or unsupported)
		old map[string]string
		// kernelLog: what console_cables.kernel_log says with the microVM profile on
		kernelLog bool
	}{
		"off":                            {false, map[string]string{"w1": ready}, false, nil, false},
		"on, no node":                    {true, nil, false, nil, false},
		"on, every relay ready":          {true, map[string]string{"w1": ready, "w2": ready}, true, nil, true},
		"on, a node of an older release": {true, map[string]string{"w1": ready, "w2": ready}, true, map[string]string{"w2": "-"}, false},
		"on, a relay without the kernel log": {true, map[string]string{"w1": ready, "w2": ready}, true,
			map[string]string{"w2": nodeagent.Unavailable(nodeagent.CodeKernelLogUnsupported, "x")}, false},
		// a ready value left by an earlier process of the service (a rollback starts a new one, with a new relay key)
		"on, a stale ready of another relay key": {true, map[string]string{"w1": ready, "w2": ready}, true,
			map[string]string{"w2": nodeagent.KernelLogReady(strings.Repeat("o", 43))}, false},
		"on, a ready without revision": {true, map[string]string{"w1": ready, "w2": ready}, true, map[string]string{"w2": "ready"}, false},
		"on, a node without relay":     {true, map[string]string{"w1": ready, "w2": "-"}, false, nil, false},
		"on, a relay not answering":    {true, map[string]string{"w1": ready, "w2": nodeagent.Unavailable(nodeagent.CodeRelayUnreachable, "x")}, false, nil, false},
		"on, a relay disabled":         {true, map[string]string{"w1": nodeagent.PreflightDisabled}, false, nil, false},
	} {
		for _, microVM := range []bool{false, true} {
			f := testFeatures
			sw := switches.Everything()
			sw.ConsoleCables = c.on
			f.Switches = &sw
			f.Profiles.MicroVM = microVM
			h := featuresHandler(t, f, newTenantTenant("a", true, nil))
			for node, ann := range c.nodes {
				n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, Labels: map[string]string{names.LabelNodeAgentReady: "true"}, Annotations: map[string]string{}},
					Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
				if ann != "-" {
					n.Annotations[names.AnnotationConsoleRelay] = ann
					n.Annotations[names.AnnotationConsoleRelayKey] = strings.Repeat("k", 43)
					n.Annotations[names.AnnotationConsoleKernelLog] = nodeagent.KernelLogReady(strings.Repeat("k", 43))
				}
				if kl, ok := c.old[node]; ok {
					delete(n.Annotations, names.AnnotationConsoleKernelLog)
					if kl != "-" {
						n.Annotations[names.AnnotationConsoleKernelLog] = kl
					}
				}
				if _, err := h.k8s.CoreV1().Nodes().Create(context.Background(), n, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
			if err != nil {
				t.Fatal(err)
			}
			cc := got.GetConsoleCables()
			if cc == nil || cc.GetEnabled() != c.want || cc.GetMaxCablesPerLab() != 64 || cc.GetMaxBytesPerSecond() != 65536 {
				t.Errorf("%s: console cables %+v", name, cc)
			}
			// the kernel log of a console port (contract 3.2) needs the lines, the microVM profile and every node's relay to know it
			if cc.GetKernelLog() != (c.kernelLog && microVM) {
				t.Errorf("%s, microvm %v: kernel_log %v", name, microVM, cc.GetKernelLog())
			}
			if k := got.GetConstants(); k.GetMaxConsolePorts() != 4 || k.GetMaxSerialPorts() != 16 {
				t.Fatalf("constants: %+v", k)
			}
		}
	}
}

func TestVerifyRelay(t *testing.T) {
	n := func(name, ann string) corev1.Node {
		x := readyNode(name, "", "")
		if ann != "" {
			x.Annotations[names.AnnotationConsoleRelay] = ann
		}
		return x
	}
	if r := verifyRelay([]corev1.Node{n("a", nodeagent.PreflightReady), n("b", nodeagent.PreflightReady)}, nil, nil); r != "" {
		t.Fatalf("all ready: %q", r)
	}
	if r := verifyRelay([]corev1.Node{n("a", nodeagent.PreflightReady), n("b", "")}, nil, nil); r != nodeagent.CodeNodeUnconfirmed+":1/2" {
		t.Fatalf("one unconfirmed: %q", r)
	}
	if r := verifyRelay(nil, nil, nil); r == "" {
		t.Fatal("no node confirms nothing")
	}
}
