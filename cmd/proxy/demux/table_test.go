package demux

import (
	"encoding/base64"
	"testing"
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
