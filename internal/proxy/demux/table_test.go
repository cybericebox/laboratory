package demux

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestComputeMac1Key_NotAllZeros(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	k, err := computeMac1Key(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range k {
		if b != 0 {
			return
		}
	}
	t.Fatal("mac1Key is all zeros")
}

func TestTable_UpdateAndDelete(t *testing.T) {
	tbl := NewTable()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	pubKey := base64.StdEncoding.EncodeToString(raw)
	if err := tbl.Update("uid-1", pubKey, "10.0.0.5:51820"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(tbl.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(tbl.entries))
	}
	if tbl.entries[0].Backend != "10.0.0.5:51820" {
		t.Fatalf("expected backend stored, got %q", tbl.entries[0].Backend)
	}
	if err := tbl.Update("uid-1", pubKey, "10.0.0.6:51820"); err != nil {
		t.Fatalf("Update (replace): %v", err)
	}
	if tbl.entries[0].Backend != "10.0.0.6:51820" {
		t.Fatalf("expected backend updated on second call, got %q", tbl.entries[0].Backend)
	}
	tbl.Delete("uid-1")
	if len(tbl.entries) != 0 {
		t.Fatal("expected 0 entries after delete")
	}
}

// R-16: the table is keyed by UID but a deletion event carries only the name: the entry of a removed group must go with it.
func TestWatcherRemovesTheEntryOfADeletedGroup(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i + 3)
	}
	lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", UID: "uid-g1"}}
	lg.Status.VPN.PublicKey, lg.Status.VPN.Registered, lg.Status.VPN.Backend = base64.StdEncoding.EncodeToString(pub), true, "10.244.2.3:51820"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lg).Build()
	w := &LabGroupWatcher{Client: c, Table: NewTable(), VPNServicePort: 51820}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "g1"}}
	if _, err := w.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(w.Table.entries) != 1 {
		t.Fatalf("entries: %d", len(w.Table.entries))
	}
	if err := c.Delete(context.Background(), lg); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(w.Table.entries) != 0 {
		t.Fatalf("the entry of the removed group leaked: %d", len(w.Table.entries))
	}
}

// The backend is parsed when the entry is made (an address, no lookup); the table hands out the parsed address.
func TestTableKeepsTheResolvedBackend(t *testing.T) {
	tbl := NewTable()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	if err := tbl.Update("u", base64.StdEncoding.EncodeToString(raw), "127.0.0.1:51820"); err != nil {
		t.Fatal(err)
	}
	if e := tbl.entries[0]; e.Resolved == nil || e.Resolved.Port != 51820 {
		t.Fatalf("resolved: %v", e.Resolved)
	}
	// a backend that is a name has no address (the handshake is dropped; nothing is looked up, not even off the read loop)
	if err := tbl.Update("v", base64.StdEncoding.EncodeToString(raw), "nonexistent.invalid:1"); err != nil {
		t.Fatal(err)
	}
	if tbl.entries[1].Resolved != nil {
		t.Fatal("unresolvable")
	}
}

// E-6: the wg-demux is ready only when every group the cache holds has been put in the table.
func TestWatcherIsSyncedOnlyAfterEveryGroupWasReconciled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *laboratoryv1alpha1.LabGroup {
		return &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mk("a"), mk("b")).Build()
	w := &LabGroupWatcher{Client: c, Table: NewTable(), VPNServicePort: 51820}
	ctx := context.Background()
	if w.Synced(ctx) {
		t.Fatal("not synced before any group was reconciled")
	}
	_, _ = w.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	if w.Synced(ctx) {
		t.Fatal("not synced with one group still to do")
	}
	_, _ = w.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "b"}})
	if !w.Synced(ctx) {
		t.Fatal("synced once every group was reconciled")
	}
}

func groupWithBackend(name, backend string) *laboratoryv1alpha1.LabGroup {
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i + 5)
	}
	lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}}
	lg.Status.VPN.PublicKey, lg.Status.VPN.Registered, lg.Status.VPN.Backend = base64.StdEncoding.EncodeToString(pub), true, backend
	return lg
}

func watcherOf(t *testing.T, lg *laboratoryv1alpha1.LabGroup) (*LabGroupWatcher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lg).Build()
	return &LabGroupWatcher{Client: c, Table: NewTable(), VPNServicePort: 51820}, c
}

func reconcileGroup(t *testing.T, w *LabGroupWatcher, name string) {
	t.Helper()
	if _, err := w.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatal(err)
	}
}

// The table is fed from the status the operator wrote: the VPN pod address, parsed, no name and no lookup.
func TestWatcherUsesTheBackendOfTheStatus(t *testing.T) {
	w, _ := watcherOf(t, groupWithBackend("g1", "10.244.2.3:51820"))
	reconcileGroup(t, w, "g1")
	e := w.Table.Entries()
	if len(e) != 1 || e[0].Backend != "10.244.2.3:51820" || e[0].Resolved == nil || e[0].Resolved.String() != "10.244.2.3:51820" {
		t.Fatalf("%+v", e)
	}
}

// A new address in the status (the VPN pod was replaced) is the one the table has after the next reconcile, with no restart, and the sessions
// that went to the old pod are retired.
func TestWatcherFollowsAChangeOfTheBackend(t *testing.T) {
	w, c := watcherOf(t, groupWithBackend("g1", "10.244.2.3:51820"))
	retired := 0
	w.Table.OnChange = func(string) { retired++ }
	reconcileGroup(t, w, "g1")
	before := retired
	var lg laboratoryv1alpha1.LabGroup
	if err := c.Get(context.Background(), types.NamespacedName{Name: "g1"}, &lg); err != nil {
		t.Fatal(err)
	}
	lg.Status.VPN.Backend = "10.244.9.9:51820"
	if err := c.Update(context.Background(), &lg); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, w, "g1")
	e := w.Table.Entries()
	if len(e) != 1 || e[0].Resolved == nil || e[0].Resolved.String() != "10.244.9.9:51820" {
		t.Fatalf("the new address is not used: %+v", e)
	}
	if retired == before {
		t.Fatal("the sessions of the old pod must be retired")
	}
}

// Fail closed: no address, or one that is not a unicast IPv4 address on the WireGuard port, leaves the group out of the table (its handshakes
// are dropped), and takes out an entry the group had.
func TestWatcherDropsTheGroupWithoutAUsableBackend(t *testing.T) {
	for _, bad := range []string{
		"", "vpn.lg-g1.svc.cluster.local:51820", "10.244.2.3", "10.244.2.3:1", "10.244.2.3:0", "10.244.2.3:notaport",
		"127.0.0.1:51820", "0.0.0.0:51820", "169.254.1.1:51820", "224.0.0.1:51820", "[fd00::1]:51820", "10.244.2.3:51820/x",
	} {
		t.Run(bad, func(t *testing.T) {
			w, c := watcherOf(t, groupWithBackend("g1", "10.244.2.3:51820"))
			reconcileGroup(t, w, "g1")
			if len(w.Table.Entries()) != 1 {
				t.Fatal("setup: no entry")
			}
			var lg laboratoryv1alpha1.LabGroup
			if err := c.Get(context.Background(), types.NamespacedName{Name: "g1"}, &lg); err != nil {
				t.Fatal(err)
			}
			lg.Status.VPN.Backend = bad
			if err := c.Update(context.Background(), &lg); err != nil {
				t.Fatal(err)
			}
			reconcileGroup(t, w, "g1")
			if n := len(w.Table.Entries()); n != 0 {
				t.Fatalf("%q must drop the group, the table has %d entries", bad, n)
			}
		})
	}
	// a group that never had a good address is not in the table either
	w, _ := watcherOf(t, groupWithBackend("g2", ""))
	reconcileGroup(t, w, "g2")
	if len(w.Table.Entries()) != 0 {
		t.Fatal("a group without an address is in the table")
	}
}

// The demux does no lookup: a name is not an address, and nothing waits for DNS.
func TestResolveBackendTakesAnIPv4AddressOnly(t *testing.T) {
	if a, err := resolveBackend(context.Background(), "10.244.2.3:51820"); err != nil || a.String() != "10.244.2.3:51820" {
		t.Fatal(a, err)
	}
	for _, bad := range []string{"vpn.lg-g1.svc.cluster.local:51820", "localhost:51820", "[::1]:51820", "10.244.2.3", "10.244.2.3:0"} {
		start := time.Now()
		if a, err := resolveBackend(context.Background(), bad); err == nil || a != nil {
			t.Errorf("%q resolved to %v", bad, a)
		}
		if time.Since(start) > 100*time.Millisecond {
			t.Errorf("%q took %v: it looked something up", bad, time.Since(start))
		}
	}
}
