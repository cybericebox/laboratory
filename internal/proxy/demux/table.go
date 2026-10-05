package demux

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/blake2s"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// Mac1Key is the 32-byte key used to compute and verify mac1 in WireGuard handshake init.
type Mac1Key [32]byte

type TableEntry struct {
	UID     string
	Mac1Key Mac1Key
	// Backend is the in-cluster Service address (host:port) for the VPN server of this group; Resolved is it resolved, kept up to
	// date by Table.RunResolver (the read loop never does a DNS lookup).
	Backend  string
	Resolved *net.UDPAddr
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
	resolved, _ := net.ResolveUDPAddr("udp4", backend) // a failure leaves it nil: RunResolver tries again
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, e := range t.entries {
		if e.UID == uid {
			t.entries[i].Mac1Key = k
			if t.entries[i].Backend != backend || resolved != nil {
				t.entries[i].Backend, t.entries[i].Resolved = backend, resolved
			}
			return nil
		}
	}
	t.entries = append(t.entries, TableEntry{UID: uid, Mac1Key: k, Backend: backend, Resolved: resolved})
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
// Returns the matched group UID and its VPN Service backend (host:port).
func (t *Table) FindByMac1(packet []byte) (uid string, backend *net.UDPAddr, found bool) {
	if len(packet) < 32 {
		return "", nil, false
	}
	msgBody := packet[:len(packet)-32]
	mac1InPkt := packet[len(packet)-32 : len(packet)-16]

	// The scan holds the read lock instead of copying the table: a copy per handshake is an allocation per group on the read loop.
	t.mu.RLock()
	defer t.mu.RUnlock()
	for i := range t.entries {
		e := &t.entries[i]
		mac := computeMAC(e.Mac1Key[:], msgBody)
		if bytesEqual(mac[:], mac1InPkt) {
			return e.UID, e.Resolved, true
		}
	}
	return "", nil, false
}

// RunResolver keeps the resolved address of every backend fresh, off the read loop, until stop is closed. A backend that cannot be
// resolved keeps its last good address.
func (t *Table) RunResolver(stop <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		t.mu.RLock()
		backends := make(map[string]string, len(t.entries))
		for _, e := range t.entries {
			backends[e.UID] = e.Backend
		}
		t.mu.RUnlock()
		for uid, backend := range backends {
			r, err := net.ResolveUDPAddr("udp4", backend)
			if err != nil {
				continue
			}
			t.mu.Lock()
			for i := range t.entries {
				if t.entries[i].UID == uid && t.entries[i].Backend == backend {
					t.entries[i].Resolved = r
				}
			}
			t.mu.Unlock()
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// computeMAC computes BLAKE2s-128 (WireGuard "MAC" function per the spec).
// Using New128 — NOT New256 truncated; the output is different.
func computeMAC(key, msg []byte) [16]byte {
	h, _ := blake2s.New128(key)
	h.Write(msg)
	var out [16]byte
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
	Table          *Table
	VPNServicePort int

	mu     sync.Mutex
	uidOfs map[string]string // LabGroup name -> UID of the entry it made: the table is keyed by UID, a deletion event carries the name
	seen   map[string]bool   // LabGroup names reconciled at least once
}

func (w *LabGroupWatcher) markSeen(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]bool{}
	}
	w.seen[name] = true
}

// Synced says whether every LabGroup the cache holds has been reconciled into the table at least once: only then does the demux know
// the groups' keys, and a handshake of a group that is not in the table is dropped. It is the readiness of the wg-demux container.
func (w *LabGroupWatcher) Synced(ctx context.Context) bool {
	var groups laboratoryv1alpha1.LabGroupList
	if err := w.List(ctx, &groups); err != nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range groups.Items {
		if !w.seen[groups.Items[i].Name] {
			return false
		}
	}
	return true
}

func (w *LabGroupWatcher) remember(name, uid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.uidOfs == nil {
		w.uidOfs = map[string]string{}
	}
	w.uidOfs[name] = uid
}

// forget removes the table entry the group made and the memory of it.
func (w *LabGroupWatcher) forget(name string) {
	w.mu.Lock()
	uid := w.uidOfs[name]
	delete(w.uidOfs, name)
	w.mu.Unlock()
	if uid != "" {
		w.Table.Delete(uid)
	}
}

func (w *LabGroupWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	defer w.markSeen(req.Name)

	var lg laboratoryv1alpha1.LabGroup
	if err := w.Get(ctx, req.NamespacedName, &lg); err != nil {
		if client.IgnoreNotFound(err) == nil {
			w.forget(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !lg.DeletionTimestamp.IsZero() {
		w.Table.Delete(string(lg.UID))
		w.forget(lg.Name)
		return ctrl.Result{}, nil
	}

	pubKey := lg.Status.VPN.PublicKey
	if pubKey == "" || !lg.Status.VPN.Registered {
		// VPN not yet ready — remove stale entry if present.
		w.Table.Delete(string(lg.UID))
		w.forget(lg.Name)
		return ctrl.Result{}, nil
	}

	ns := laboratoryv1alpha1.LabGroupNamespaceOf(&lg)
	backend := fmt.Sprintf("vpn.%s.svc.cluster.local:%d", ns, w.VPNServicePort)
	log.Info("updating demux table", "group", lg.Name, "backend", backend)
	if err := w.Table.Update(string(lg.UID), pubKey, backend); err != nil {
		return ctrl.Result{}, err
	}
	w.remember(lg.Name, string(lg.UID))
	return ctrl.Result{}, nil
}

func (w *LabGroupWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroup{}).
		Complete(reconcileutil.Quiet(w))
}
