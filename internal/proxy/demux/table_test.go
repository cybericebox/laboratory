package demux

import (
	"context"
	"encoding/base64"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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
	lg.Status.VPN.PublicKey, lg.Status.VPN.Registered = base64.StdEncoding.EncodeToString(pub), true
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

// The backend is resolved when the entry is made and kept fresh off the read loop; the table hands out the resolved address.
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
	// an unresolvable backend leaves the address empty (the handshake is dropped, not looked up on the read loop)
	if err := tbl.Update("v", base64.StdEncoding.EncodeToString(raw), "nonexistent.invalid:1"); err != nil {
		t.Fatal(err)
	}
	if tbl.entries[1].Resolved != nil {
		t.Fatal("unresolvable")
	}
}
