package vniledger

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newLedger(t *testing.T, c client.Client, node string) *Ledger {
	t.Helper()
	return &Ledger{Client: c, Reader: c, Namespace: "ns", Node: node}
}

func TestLedgerNoteClearAndRestart(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	ctx := context.Background()
	l := newLedger(t, c, "n1")
	if err := l.Note(7); err != nil {
		t.Fatal(err)
	}
	if err := l.Note(0); err != nil { // index 0 is a VNI like any other
		t.Fatal(err)
	}
	if err := l.Note(7); err != nil {
		t.Fatal(err)
	}
	// stored before Note returned: a fresh ledger (the node-agent restarted) sees both
	again := newLedger(t, c, "n1")
	got, err := again.Entries(ctx)
	if err != nil || len(got) != 2 || got[0] != 0 || got[1] != 7 {
		t.Fatalf("entries after restart = %v, %v", got, err)
	}
	if err := again.Clear(7); err != nil {
		t.Fatal(err)
	}
	snap, err := Read(ctx, c, "ns")
	if err != nil || len(snap.Held[7]) != 0 || len(snap.Held[0]) != 1 || snap.Held[0][0] != "n1" || len(snap.Nodes) != 1 {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if err := again.Clear(99); err != nil { // clearing what is not there is fine
		t.Fatal(err)
	}
}

func TestLedgerNoteFailsWhenItCannotBeStored(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	base := fake.NewClientBuilder().WithScheme(s).Build()
	l := newLedger(t, failingPatch{base}, "n1")
	if err := l.Note(7); err == nil {
		t.Fatal("a VNI that was not recorded must not be reported as recorded")
	}
	if entries, _ := l.Entries(context.Background()); len(entries) != 0 {
		t.Fatalf("a failed Note left an entry: %v", entries)
	}
}

type failingPatch struct{ client.Client }

func (failingPatch) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return context.DeadlineExceeded
}

func TestReadIgnoresOtherConfigMaps(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	other := &corev1.ConfigMap{}
	other.Name, other.Namespace, other.Data = "laboratory-bandwidth", "ns", map[string]string{"x": "y"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(other).Build()
	snap, err := Read(context.Background(), c, "ns")
	if err != nil || len(snap.Held) != 0 {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
}
