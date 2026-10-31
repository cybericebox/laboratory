package nodeagent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

type countedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *countedLog) logger() logr.Logger {
	return funcr.New(func(prefix, args string) { c.mu.Lock(); c.lines = append(c.lines, args); c.mu.Unlock() }, funcr.Options{})
}

// A Connection that cannot be programmed says why in its status (condition Wired, a fixed reason), the status is written only when the
// reason or the message changes, one log line is written per change of reason, and the condition turns True when the ports are programmed.
func TestMarkWiredMakesTheWaitVisible(t *testing.T) {
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	conn := &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Generation: 1}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(conn).WithObjects(conn).Build()
	ctx := context.Background()
	var lg countedLog
	log := lg.logger()
	read := func() *metav1.Condition {
		var got laboratoryv1alpha1.Connection
		if err := c.Get(ctx, types.NamespacedName{Name: "c", Namespace: "ns"}, &got); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, ConditionWired)
	}

	for i := 0; i < 5; i++ { // five retries for the same reason: one line, one write
		if err := markWired(ctx, c, log, conn, false, WaitOVSPort, "endpoint vpn/eth0: "+waitMessage(WaitOVSPort)); err != nil {
			t.Fatal(err)
		}
	}
	cond := read()
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != WaitOVSPort || !strings.Contains(cond.Message, "vpn/eth0") {
		t.Fatalf("condition %+v", cond)
	}
	if len(lg.lines) != 1 {
		t.Fatalf("one log line for one reason, got %d: %v", len(lg.lines), lg.lines)
	}

	if err := markWired(ctx, c, log, conn, false, WaitInvalidNetworkIdx, waitMessage(WaitInvalidNetworkIdx)); err != nil {
		t.Fatal(err)
	}
	if read().Reason != WaitInvalidNetworkIdx || len(lg.lines) != 2 {
		t.Fatalf("a new reason is written and logged: %+v %v", read(), lg.lines)
	}

	if err := markWired(ctx, c, log, conn, true, ReasonProgrammed, "the ports are programmed"); err != nil {
		t.Fatal(err)
	}
	if cond := read(); cond.Status != metav1.ConditionTrue || cond.Reason != ReasonProgrammed {
		t.Fatalf("programmed: %+v", cond)
	}
	for _, l := range lg.lines {
		if strings.Contains(l, "/var/") || strings.Contains(l, "sock") {
			t.Errorf("no path in a log line: %s", l)
		}
	}
}

// Lab network 0 (and anything above 254) has no interface in the VPN or gateway pod: it is a named reason, not a silent wait.
func TestLabNetworkIndexRange(t *testing.T) {
	for n, want := range map[uint]bool{0: false, 1: true, 254: true, 255: false} {
		if validLabIndex(n) != want {
			t.Errorf("validLabIndex(%d) = %v", n, !want)
		}
	}
	for _, r := range []string{WaitEndpointNode, WaitVNI, WaitNetworkObject, WaitInvalidNetworkIdx, WaitDevicePod, WaitOVSPort} {
		if waitMessage(r) == "" || strings.ContainsAny(r, " /:") {
			t.Errorf("reason %q must be a code", r)
		}
	}
}
