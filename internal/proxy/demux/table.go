package demux

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"

	"golang.org/x/crypto/blake2s"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Mac1Key is the 32-byte key used to compute and verify mac1 in WireGuard handshake init.
type Mac1Key [32]byte

type TableEntry struct {
	UID     string
	Mac1Key Mac1Key
	// Backend is the routable target (podIP:port) for the VPN server of this
	// group. Empty when the operator has not yet observed a Running pod.
	Backend string
}

// Table is an in-memory mac1_key→UID/backend mapping, rebuilt from LabGroup watches.
// Brute-force scan is O(n) but handshake init is rare (~once per 2 min per peer).
type Table struct {
	mu      sync.RWMutex
	entries []TableEntry
}

func NewTable() *Table { return &Table{} }

// computeMac1Key computes BLAKE2s("mac1----" || pubKeyBytes).
// pubKeyBase64 is a WireGuard base64-encoded 32-byte public key.
func computeMac1Key(pubKeyBase64 string) (Mac1Key, error) {
	b, err := base64.StdEncoding.DecodeString(pubKeyBase64)
	if err != nil || len(b) != 32 {
		return Mac1Key{}, fmt.Errorf("invalid pubkey %q", pubKeyBase64)
	}
	label := []byte("mac1----")
	h, err := blake2s.New256(nil)
	if err != nil {
		return Mac1Key{}, fmt.Errorf("blake2s: %w", err)
	}
	h.Write(label)
	h.Write(b)
	var key Mac1Key
	copy(key[:], h.Sum(nil))
	return key, nil
}

// Update inserts or replaces the entry for uid.
func (t *Table) Update(uid, pubKey, backend string) error {
	k, err := computeMac1Key(pubKey)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, e := range t.entries {
		if e.UID == uid {
			t.entries[i].Mac1Key = k
			t.entries[i].Backend = backend
			return nil
		}
	}
	t.entries = append(t.entries, TableEntry{UID: uid, Mac1Key: k, Backend: backend})
	return nil
}

// Delete removes the entry for uid.
func (t *Table) Delete(uid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, e := range t.entries {
		if e.UID == uid {
			t.entries = append(t.entries[:i], t.entries[i+1:]...)
			return
		}
	}
}

// FindByMac1 brute-forces mac1 verification.
// packet is the raw WireGuard type-1 packet (UDP payload).
// mac1 occupies bytes [len-32 : len-16]; the message body for MAC is packet[:len-32].
// Returns the matched group UID and its registered backend (podIP:port); backend
// is empty when the operator has not yet seen a Running VPN pod.
func (t *Table) FindByMac1(packet []byte) (uid, backend string, found bool) {
	if len(packet) < 32 {
		return "", "", false
	}
	msgBody := packet[:len(packet)-32]
	mac1InPkt := packet[len(packet)-32 : len(packet)-16]

	t.mu.RLock()
	entries := make([]TableEntry, len(t.entries))
	copy(entries, t.entries)
	t.mu.RUnlock()

	for _, e := range entries {
		mac := computeMAC(e.Mac1Key[:], msgBody)
		if bytesEqual(mac[:16], mac1InPkt) {
			return e.UID, e.Backend, true
		}
	}
	return "", "", false
}

func computeMAC(key, msg []byte) [32]byte {
	h, _ := blake2s.New256(key)
	h.Write(msg)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// LabGroupWatcher is a controller-runtime reconciler keeping Table in sync with LabGroups.
type LabGroupWatcher struct {
	client.Client
	Table *Table
}

func (w *LabGroupWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lg laboratoryv1alpha1.LabGroup
	if err := w.Get(ctx, req.NamespacedName, &lg); err != nil {
		if client.IgnoreNotFound(err) == nil {
			w.Table.Delete(req.Name) // use name as UID fallback on 404
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lg.DeletionTimestamp.IsZero() {
		w.Table.Delete(string(lg.UID))
		return ctrl.Result{}, nil
	}
	if lg.Status.VPN.PublicKey == "" {
		return ctrl.Result{}, nil
	}
	if err := w.Table.Update(string(lg.UID), lg.Status.VPN.PublicKey, lg.Status.VPN.Backend); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (w *LabGroupWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroup{}).
		Complete(w)
}
