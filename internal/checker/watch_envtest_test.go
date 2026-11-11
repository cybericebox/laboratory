package checker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
)

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func checksSecret(t *testing.T, ns, lab string, expect string) *corev1.Secret {
	t.Helper()
	raw, err := json.Marshal(labchecks.ChecksDoc{Checks: []labchecks.Check{{ID: "c", Device: "web", Mode: "continuous", ScheduleSeconds: 1, ManualRun: true, TimeLimitSeconds: 1,
		Parts: []labchecks.Part{{File: &labchecks.FilePart{Path: "/f", ContentContains: &expect}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lab + labchecks.ChecksSecretSuffix, Namespace: ns}, Data: map[string][]byte{labchecks.ChecksKey: raw}}
}

func devicePod(ns, lab, device, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + device + "-" + node, Namespace: ns, Labels: map[string]string{names.LabelLab: lab, names.LabelDevice: device}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: device, Image: "busybox"}}},
	}
}

// The whole chain on a real API server: informers (the pods of this node only), the checks Secret read on demand and again after a
// rewrite, the schedule, the manual run with its acknowledgement, and the results applied into the Lab status.
func TestCheckerEndToEnd(t *testing.T) {
	c := startEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lab := makeLab(t, c, "e2e", "c")
	ns := lab.Namespace
	if err := c.Create(ctx, checksSecret(t, ns, lab.Name, "on")); err != nil {
		t.Fatal(err)
	}
	mine, other := devicePod(ns, lab.Name, "web", "node-a"), devicePod(ns, lab.Name, "db", "node-b")
	for _, p := range []*corev1.Pod{mine, other} {
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	byObject, err := CacheOptions("node-a")
	if err != nil {
		t.Fatal(err)
	}
	// envtest's rest config is inside the client: build the cache from the same server.
	informers, err := cache.New(testRestConfig, cache.Options{Scheme: scheme.Scheme, ByObject: byObject})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = informers.Start(ctx) }()
	if !informers.WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	backend := &fakeBackend{obs: &fakeObserver{files: map[string]FileFacts{"/f": present("service on")}}}
	engine := NewEngine(Config{Backend: backend, Writer: &StatusWriter{Client: c}, MaxConcurrent: 2, FirstRunWindow: time.Millisecond, Log: logr.Discard()})
	w := &Watcher{Engine: engine, Reader: c, Log: logr.Discard(), Retry: 100 * time.Millisecond}
	if err := w.Start(ctx, informers); err != nil {
		t.Fatal(err)
	}
	go engine.Run(ctx, nil)

	state := func() laboratoryv1alpha1.CheckResult { return results(getLab(t, c, lab))["c"] }
	eventually(t, "the check passes", 30*time.Second, func() bool { return state().State == laboratoryv1alpha1.CheckPassed })
	first := state()
	if first.FirstPassedMs == 0 || first.ChangedMs == 0 {
		t.Fatalf("got %+v", first)
	}
	// The pod of the other node was never looked at.
	backend.mu.Lock()
	opened, others := backend.opened, backend.devices["db"]
	backend.mu.Unlock()
	if opened == 0 || others != 0 {
		t.Fatalf("opened %d times, %d of them the device of another node", opened, others)
	}

	// Content changes: not passed, firstPassedMs kept.
	backend.obs.set("/f", present("service off"))
	eventually(t, "not passed", 30*time.Second, func() bool { return state().State == laboratoryv1alpha1.CheckNotPassed })
	if got := state(); got.FirstPassedMs != first.FirstPassedMs || got.Detail == "" {
		t.Fatalf("got %+v", got)
	}

	// The agent rewrites the Secret under its claim: the expected value changes and the checker reads it again.
	var cur laboratoryv1alpha1.Lab
	key := client.ObjectKeyFromObject(lab)
	if err := c.Get(ctx, key, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Annotations = map[string]string{names.AnnotationLabVariableAdmission: "token"}
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, checksSecret(t, ns, lab.Name, "")); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, checksSecret(t, ns, lab.Name, "off")); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &cur); err != nil {
		t.Fatal(err)
	}
	delete(cur.Annotations, names.AnnotationLabVariableAdmission)
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	eventually(t, "passes against the rewritten expectation", 30*time.Second, func() bool { return state().State == laboratoryv1alpha1.CheckPassed })

	// A manual run is taken and acknowledged.
	if err := c.Get(ctx, key, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Annotations = map[string]string{names.AnnotationRunChecks: `{"token":3}`}
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the acknowledgement", 30*time.Second, func() bool { return getLab(t, c, lab).Status.ChecksRunAck == 3 })

	// The pod goes NotReady: no run is made and the result is kept.
	opened = backend.opened
	mine.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := c.Status().Update(ctx, mine); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	settled := backend.opened
	time.Sleep(3 * time.Second)
	if backend.opened != settled || settled > opened+1 {
		t.Fatalf("runs continued on a device that is not ready: %d -> %d -> %d", opened, settled, backend.opened)
	}
	if state().State != laboratoryv1alpha1.CheckPassed {
		t.Fatalf("the result was changed: %+v", state())
	}
}

func TestKeepaliveLease(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sys"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	state := "ready"
	k := &Keepalive{Client: c, Namespace: "sys", Node: "node-a", Holder: "checker-xyz", Version: "v1", State: func() string { return state }, Now: func() time.Time { return now }}
	if err := k.Beat(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	state = "unavailable: STATE_DIR_MISSING: x"
	if err := k.Beat(ctx); err != nil {
		t.Fatal(err)
	}
	other := &Keepalive{Client: c, Namespace: "sys", Node: "node-b", Holder: "checker-abc", State: func() string { return "ready" }, Now: func() time.Time { return now }}
	if err := other.Beat(ctx); err != nil {
		t.Fatal(err)
	}
	var leases coordLeaseList
	if err := c.List(ctx, &leases.list, client.InNamespace("sys"), client.MatchingLabels{LabelChecker: "true"}); err != nil {
		t.Fatal(err)
	}
	if len(leases.list.Items) != 2 {
		t.Fatalf("one Lease per node, got %d", len(leases.list.Items))
	}
	for _, l := range leases.list.Items {
		if l.Name == LeaseName("node-a") {
			if l.Spec.RenewTime == nil || !l.Spec.RenewTime.Time.Equal(now) || *l.Spec.HolderIdentity != "checker-xyz" || l.Annotations[AnnotationState] != state || *l.Spec.LeaseDurationSeconds != 60 {
				t.Fatalf("got %+v", l)
			}
		}
	}
}
