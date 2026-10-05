//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"testing"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cybericebox/laboratory/internal/accessroute"
	"github.com/cybericebox/laboratory/internal/names"
)

func accessPod(name, node string, caps ...corev1.Capability) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", UID: "uid-1",
			Labels:      map[string]string{names.LabelLab: "lab", names.LabelDevice: name},
			Annotations: map[string]string{AnnotationDefaultNetwork: names.AccessPortIface},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:            "device",
				SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: caps}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestNeedsAccessPortCheck(t *testing.T) {
	if !needsAccessPortCheck(accessPod("a", "n1", "NET_ADMIN"), "n1") {
		t.Error("a pod with NET_ADMIN must be checked")
	}
	if needsAccessPortCheck(accessPod("a", "n1", "NET_RAW"), "n1") {
		t.Error("a pod without NET_ADMIN cannot change its routes and is left alone")
	}
	if needsAccessPortCheck(accessPod("a", "n2", "NET_ADMIN"), "n1") {
		t.Error("a pod of another node is not ours")
	}
	p := accessPod("a", "n1", "NET_ADMIN")
	p.Annotations[AnnotationDefaultNetwork] = ""
	if needsAccessPortCheck(p, "n1") {
		t.Error("a pod without the access port has nothing to check")
	}
}

// A flushed table is restored from the routes seen before; a lost port is reported once, as one Warning event.
func TestAccessPortCheckerRestoresAndReportsOnce(t *testing.T) {
	pod := accessPod("dev", "n1", "NET_ADMIN")
	rec := record.NewFakeRecorder(10)
	known := []netlink.Route{{LinkIndex: 7, Table: names.AccessPortRouteTable}}
	var calls [][]netlink.Route
	var answer error
	c := &AccessPortChecker{
		Client:   fake.NewClientBuilder().WithObjects(pod).Build(),
		NodeName: "n1",
		Recorder: rec,
		netnsOf:  func(context.Context, string) (string, error) { return "/proc/1/ns/net", nil },
		ensure: func(_, _ string, k []netlink.Route) (accessroute.Result, error) {
			calls = append(calls, k)
			if answer != nil {
				return accessroute.Result{}, answer
			}
			return accessroute.Result{Routes: known, Changed: len(k) > 0}, nil
		},
	}
	ctx := context.Background()
	c.Check(ctx)
	c.Check(ctx)
	if len(calls) != 2 || len(calls[0]) != 0 || len(calls[1]) != 1 {
		t.Fatalf("the routes seen in the first round must come back in the second: %v", calls)
	}

	answer = fmt.Errorf("find accessport: %w", accessroute.ErrNoPort)
	c.Check(ctx)
	c.Check(ctx)
	if got := len(rec.Events); got != 1 {
		t.Fatalf("a lost port is reported once, got %d events", got)
	}
	answer = nil
	c.Check(ctx)
	answer = accessroute.ErrNoPort
	c.Check(ctx)
	if got := len(rec.Events); got != 2 {
		t.Fatalf("a port lost again after it came back is reported again, got %d events", got)
	}
}
