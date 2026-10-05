package errorlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestAggregatorGroupsCountsAndKeepsThreeSamples(t *testing.T) {
	a := New("operator", "pod-1")
	for i := 0; i < 5; i++ {
		a.Record("reconcile", fmt.Sprintf("update lab team-%d/web: conflict (attempt %d) from 10.0.0.%d", i, i, i))
	}
	a.Record("reconcile", "a different failure")
	snap := a.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(snap), snap)
	}
	var conflict Group
	for _, g := range snap {
		if g.Total == 5 {
			conflict = g
		}
	}
	if conflict.Total != 5 || len(conflict.Samples) != MaxSamples {
		t.Fatalf("%+v", conflict)
	}
	for _, s := range conflict.Samples {
		if strings.Contains(s, "10.0.0.") || strings.Contains(s, "team-") {
			t.Errorf("sample leaks: %q", s)
		}
	}
	if len(a.Dirty()) != 2 || len(a.Dirty()) != 0 {
		t.Fatal("Dirty returns the changed groups once")
	}
	a.Record("reconcile", "a different failure")
	if d := a.Dirty(); len(d) != 1 || d[0].Total != 2 {
		t.Fatalf("only the changed group: %+v", d)
	}
}

func TestAggregatorIsBounded(t *testing.T) {
	a := New("node-agent", "n1")
	for i := 0; i < MaxGroups+50; i++ {
		a.Record("x", strings.Repeat("e", 3)+strings.Repeat("-", 1)+fmt.Sprintf("kind%c%c%c", 'a'+rune(i%26), 'a'+rune(i/26%26), 'a'+rune(i/676)))
	}
	if n := len(a.Snapshot()); n > MaxGroups+1 {
		t.Fatalf("groups = %d, the cap is %d (+ the overflow group)", n, MaxGroups)
	}
	over := 0
	for _, g := range a.Snapshot() {
		if g.Kind == OverflowKind {
			over = int(g.Total)
		}
	}
	if over == 0 {
		t.Fatal("errors past the cap are counted in the overflow group")
	}
}

func TestLogSinkCountsErrorsOnlyAndPassesEverythingOn(t *testing.T) {
	var lines []string
	base := funcr.New(func(prefix, args string) { lines = append(lines, prefix+args) }, funcr.Options{})
	a := New("operator", "p")
	log := WrapLogger(base, a).WithName("labgroup")
	log.Info("fine")
	log.Error(errors.New("boom 42 at 10.1.1.1"), "reconcile failed")
	log.WithValues("k", "v").Error(nil, "second")
	if len(lines) != 3 {
		t.Fatalf("every line reaches the real logger: %v", lines)
	}
	snap := a.Snapshot()
	if len(snap) != 2 || snap[0].Kind != "labgroup" {
		t.Fatalf("%+v", snap)
	}
	for _, g := range snap {
		if strings.Contains(g.Normalized, "10.1.1.1") {
			t.Fatalf("address in %q", g.Normalized)
		}
	}
	var _ = log
}

func TestPublisherWritesAndUpdatesEventsAndTheyParseBack(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	a := New("node-agent", "node-7")
	a.Record("ovs", "add port failed: exit status 1")
	p := &Publisher{Client: cs, Namespace: "laboratory-system", Agg: a}
	p.Flush(context.Background())
	evs, _ := cs.CoreV1().Events("laboratory-system").List(context.Background(), metav1.ListOptions{LabelSelector: Label + "=true"})
	if len(evs.Items) != 1 {
		t.Fatalf("events = %d", len(evs.Items))
	}
	o, ok := ParseEvent(&evs.Items[0])
	if !ok || o.Component != "node-agent" || o.Instance != "node-7" || o.Total != 1 || o.Kind != "ovs" || len(o.Samples) != 1 {
		t.Fatalf("%+v %v", o, ok)
	}
	// more of the same error: the same event is updated, not a new one made
	a.Record("ovs", "add port failed: exit status 1")
	a.Record("ovs", "add port failed: exit status 1")
	p.Flush(context.Background())
	evs, _ = cs.CoreV1().Events("laboratory-system").List(context.Background(), metav1.ListOptions{})
	if len(evs.Items) != 1 {
		t.Fatalf("one event per fingerprint: %d", len(evs.Items))
	}
	if o, _ := ParseEvent(&evs.Items[0]); o.Total != 3 || evs.Items[0].Count != 3 {
		t.Fatalf("total %d count %d", o.Total, evs.Items[0].Count)
	}
	// nothing changed: nothing is written
	p.Flush(context.Background())
	// ParseEvent refuses what is not a journal event
	if _, ok := ParseEvent(&evs.Items[0]); !ok {
		t.Fatal("still parses")
	}
	other := evs.Items[0].DeepCopy()
	other.Reason = "Pulled"
	if _, ok := ParseEvent(other); ok {
		t.Fatal("an ordinary event is not a journal event")
	}
}

// What a reader takes from an event is redacted again, whatever the writer did.
func TestParseEventDoesNotTrustTheWriter(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	a := New("operator", "p")
	a.Record("x", "boom")
	(&Publisher{Client: cs, Namespace: "ns", Agg: a}).Flush(context.Background())
	evs, _ := cs.CoreV1().Events("ns").List(context.Background(), metav1.ListOptions{})
	ev := evs.Items[0].DeepCopy()
	ev.Annotations[annSamples] = `["password=hunter2 from 192.168.1.1 for alice@example.com"]`
	o, ok := ParseEvent(ev)
	if !ok || strings.Contains(o.Samples[0], "hunter2") || strings.Contains(o.Samples[0], "192.168") || strings.Contains(o.Samples[0], "alice") {
		t.Fatalf("%+v", o)
	}
}

func TestPublisherRunFlushesAtTheEnd(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	a := New("l7-proxy", "p-1")
	p := &Publisher{Client: cs, Namespace: "ns", Agg: a, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	a.Record("tls", "handshake failure")
	cancel()
	<-done
	if evs, _ := cs.CoreV1().Events("ns").List(context.Background(), metav1.ListOptions{}); len(evs.Items) != 1 {
		t.Fatalf("the last errors are published when the process stops: %d", len(evs.Items))
	}
}

func TestLineWriterCountsErrorLinesAndPassesAll(t *testing.T) {
	var out strings.Builder
	a := New("agent", "a-0")
	w := NewLineWriter(a, &out)
	_, _ = w.Write([]byte("2026/10/02 12:00:00 listening on :5454\n2026/10/02 12:00:01 build server: failed for 10.1.2.3\n"))
	if !strings.Contains(out.String(), "listening") || !strings.Contains(out.String(), "failed") {
		t.Fatalf("every line is passed on: %q", out.String())
	}
	snap := a.Snapshot()
	if len(snap) != 1 || snap[0].Total != 1 || strings.Contains(snap[0].Samples[0], "10.1.2.3") {
		t.Fatalf("%+v", snap)
	}
}

func TestLogSinkKeepsConflictsOutOfTheJournal(t *testing.T) {
	var lines []string
	base := funcr.New(func(prefix, args string) { lines = append(lines, prefix+args) }, funcr.Options{})
	a := New("operator", "p")
	log := WrapLogger(base, a).WithName("labgroup")
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "pools"}, "p", errors.New("the object has been modified; please apply your changes to the latest version"))
	log.Error(fmt.Errorf("wrapped: %w", conflict), "failed to sync pool state label")
	if len(a.Snapshot()) != 0 {
		t.Fatalf("a conflict must not reach the journal: %+v", a.Snapshot())
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "will retry") {
		t.Fatalf("a conflict is still logged for the trace: %v", lines)
	}
}
