package checker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// envtestAssets finds the API server binaries: KUBEBUILDER_ASSETS, else bin/k8s of this checkout or of the main checkout of the
// repository (a worktree lives in .worktrees/<name> below it).
func envtestAssets(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	for _, pattern := range []string{"../../bin/k8s/*", "../../../../bin/k8s/*"} {
		if dirs, _ := filepath.Glob(pattern); len(dirs) > 0 {
			return dirs[len(dirs)-1]
		}
	}
	t.Skip("no envtest assets: set KUBEBUILDER_ASSETS or run make setup-envtest")
	return ""
}

func startEnv(t *testing.T) client.Client {
	t.Helper()
	env := &envtest.Environment{
		BinaryAssetsDirectory: envtestAssets(t),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	testRestConfig = cfg
	if err := laboratoryv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func makeLab(t *testing.T, c client.Client, name string, ids ...string) *laboratoryv1alpha1.Lab {
	t.Helper()
	ctx := context.Background()
	ns := "ns-" + name
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	lab.Spec.Checks = &laboratoryv1alpha1.LabChecksSpec{}
	for _, id := range ids {
		lab.Spec.Checks.Items = append(lab.Spec.Checks.Items, laboratoryv1alpha1.CheckRef{ID: id, Device: "web", ManualRun: true})
	}
	if err := c.Create(ctx, lab); err != nil {
		t.Fatal(err)
	}
	return lab
}

func getLab(t *testing.T, c client.Client, lab *laboratoryv1alpha1.Lab) *laboratoryv1alpha1.Lab {
	t.Helper()
	var got laboratoryv1alpha1.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(lab), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func results(l *laboratoryv1alpha1.Lab) map[string]laboratoryv1alpha1.CheckResult {
	m := map[string]laboratoryv1alpha1.CheckResult{}
	for _, r := range l.Status.Checks {
		m[r.ID] = r
	}
	return m
}

// The status is written by two parties that must never fight: the lab controller (plain updates of the whole status object) and
// the checker (server-side apply of single entries). Proved against a real API server.
func TestStatusWriteCoexistsWithTheController(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	w := &StatusWriter{Client: c}
	lab := makeLab(t, c, "lab1", "a", "b")
	key := LabKey{Namespace: lab.Namespace, Name: lab.Name}

	// The controller writes its fields.
	cur := getLab(t, c, lab)
	cur.Status.Phase = laboratoryv1alpha1.PhaseReady
	cur.Status.VPN.Ready = true
	cur.Status.Devices = []laboratoryv1alpha1.DeviceRef{{Name: "web", Ready: true}}
	if err := c.Status().Update(ctx, cur); err != nil {
		t.Fatal(err)
	}

	// The checker writes two entries.
	if err := w.PublishCheck(ctx, key, laboratoryv1alpha1.CheckResult{ID: "a", State: laboratoryv1alpha1.CheckPassed, FirstPassedMs: 100, ChangedMs: 100}); err != nil {
		t.Fatal(err)
	}
	if err := w.PublishCheck(ctx, key, laboratoryv1alpha1.CheckResult{ID: "b", State: laboratoryv1alpha1.CheckNotPassed, ChangedMs: 120, Detail: "part 1 (file): exists: the file is missing"}); err != nil {
		t.Fatal(err)
	}
	got := getLab(t, c, lab)
	if got.Status.Phase != laboratoryv1alpha1.PhaseReady || !got.Status.VPN.Ready || len(got.Status.Devices) != 1 {
		t.Fatalf("the checker's apply touched the controller's fields: %+v", got.Status)
	}
	if r := results(got); len(r) != 2 || r["a"].State != laboratoryv1alpha1.CheckPassed || r["a"].FirstPassedMs != 100 || r["b"].Detail == "" {
		t.Fatalf("got %+v", got.Status.Checks)
	}

	// The controller updates the whole object again, from a fresh read: the entries stay.
	cur = getLab(t, c, lab)
	cur.Status.Phase = laboratoryv1alpha1.PhaseSuspended
	if err := c.Status().Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if r := results(getLab(t, c, lab)); len(r) != 2 || r["a"].State != laboratoryv1alpha1.CheckPassed {
		t.Fatalf("the controller's update dropped entries: %+v", r)
	}

	// A controller that updates from a read made BEFORE the checker's write is refused (conflict), never allowed to drop an entry.
	stale := getLab(t, c, lab)
	if err := w.PublishCheck(ctx, key, laboratoryv1alpha1.CheckResult{ID: "a", State: laboratoryv1alpha1.CheckNotPassed, FirstPassedMs: 100, ChangedMs: 200}); err != nil {
		t.Fatal(err)
	}
	stale.Status.Phase = laboratoryv1alpha1.PhaseReady
	if err := c.Status().Update(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("a stale controller update must conflict, got %v", err)
	}

	// The checker's next apply, with optional fields now empty, removes them; firstPassedMs it still sends stays.
	if err := w.PublishCheck(ctx, key, laboratoryv1alpha1.CheckResult{ID: "b", State: laboratoryv1alpha1.CheckPassed, FirstPassedMs: 300, ChangedMs: 300}); err != nil {
		t.Fatal(err)
	}
	got = getLab(t, c, lab)
	if r := results(got)["b"]; r.State != laboratoryv1alpha1.CheckPassed || r.Detail != "" || r.FirstPassedMs != 300 {
		t.Fatalf("got %+v", r)
	}
	if r := results(got)["a"]; r.State != laboratoryv1alpha1.CheckNotPassed || r.FirstPassedMs != 100 {
		t.Fatalf("entry a must be unaffected by the apply of b: %+v", r)
	}
	if got.Status.Phase != laboratoryv1alpha1.PhaseSuspended {
		t.Fatalf("phase %q", got.Status.Phase)
	}

	// The acknowledgement is its own field, written by its own manager, and touches nothing else.
	if err := w.AckRun(ctx, key, 4); err != nil {
		t.Fatal(err)
	}
	if err := w.AckRun(ctx, key, 6); err != nil {
		t.Fatal(err)
	}
	got = getLab(t, c, lab)
	if got.Status.ChecksRunAck != 6 || len(got.Status.Checks) != 2 || got.Status.Phase != laboratoryv1alpha1.PhaseSuspended {
		t.Fatalf("got %+v", got.Status)
	}
	// The spec and the metadata of the lab are not changed by the checker.
	if got.Generation != lab.Generation {
		t.Fatalf("generation moved from %d to %d", lab.Generation, got.Generation)
	}
}

// Many writes from both sides at once: nothing is lost on either side.
func TestStatusWritesInParallelWithTheController(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	w := &StatusWriter{Client: c}
	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, fmt.Sprintf("c%02d", i))
	}
	lab := makeLab(t, c, "lab2", ids...)
	key := LabKey{Namespace: lab.Namespace, Name: lab.Name}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the checker: every check changes three times
		defer wg.Done()
		for round := 1; round <= 3; round++ {
			for _, id := range ids {
				state := laboratoryv1alpha1.CheckNotPassed
				if round == 3 {
					state = laboratoryv1alpha1.CheckPassed
				}
				if err := w.PublishCheck(ctx, key, laboratoryv1alpha1.CheckResult{ID: id, State: state, ChangedMs: int64(round)}); err != nil {
					t.Errorf("publish %s: %v", id, err)
					return
				}
			}
		}
	}()
	go func() { // the controller: read, change, update, retrying on conflict as controllers do
		defer wg.Done()
		for i := 0; i < 40; i++ {
			for attempt := 0; attempt < 50; attempt++ {
				cur := getLab(t, c, lab)
				cur.Status.Phase = []laboratoryv1alpha1.Phase{laboratoryv1alpha1.PhaseProvisioning, laboratoryv1alpha1.PhaseReady}[i%2]
				cur.Status.ImageWarning = fmt.Sprintf("w%d", i)
				err := c.Status().Update(ctx, cur)
				if err == nil {
					break
				}
				if !apierrors.IsConflict(err) {
					t.Errorf("controller update: %v", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	wg.Wait()
	got := getLab(t, c, lab)
	r := results(got)
	if len(r) != 20 {
		t.Fatalf("%d entries, want 20", len(r))
	}
	for _, id := range ids {
		if r[id].State != laboratoryv1alpha1.CheckPassed || r[id].ChangedMs != 3 {
			t.Errorf("%s: %+v", id, r[id])
		}
	}
	if got.Status.Phase != laboratoryv1alpha1.PhaseReady || got.Status.ImageWarning != "w39" {
		t.Fatalf("the controller's last write is gone: %q %q", got.Status.Phase, got.Status.ImageWarning)
	}
}
