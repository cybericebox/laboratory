package demux

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"golang.org/x/crypto/blake2s"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// Mac1Key is the 32-byte key used to compute and verify mac1 in WireGuard handshake init.
type Mac1Key [32]byte

type TableEntry struct {
	UID       string
	PublicKey string
	Mac1Key   Mac1Key
	// Backend is the VPN pod address (ip:port) of this group, from LabGroup.status.vpn.backend; Resolved is it parsed.
	Backend  string
	Resolved *net.UDPAddr
}

// Table is an in-memory mac1_key→UID/backend mapping, rebuilt from LabGroup watches.
// Brute-force scan is O(n) but handshake init is rare (~once per 2 min per peer).
type Table struct {
	updates  sync.Mutex
	OnChange func(string) // invoked under table lock; callback must not enter Table
	resolve  func(context.Context, string) (*net.UDPAddr, error)
	mu       sync.RWMutex
	entries  []TableEntry
}

func NewTable() *Table { return &Table{resolve: resolveBackend} }

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

// resolveBackend parses the backend of a group: the IPv4 address and UDP port of its VPN pod, as the operator published them in
// LabGroup.status.vpn.backend. There is no name to look up: the demux does not use cluster DNS.
func resolveBackend(_ context.Context, backend string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(backend)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid backend port")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is4() {
		return nil, fmt.Errorf("the backend must be an IPv4 address")
	}
	return &net.UDPAddr{IP: net.IP(addr.AsSlice()), Port: p}, nil
}

// Update inserts or replaces the entry for uid. A backend that is not an address leaves the entry without one (a handshake of the group is
// dropped until a good address comes), and an entry that changes in any way retires the sessions of the old one.
func (t *Table) Update(uid, pubKey, backend string) error {
	t.updates.Lock()
	defer t.updates.Unlock()
	t.mu.RLock()
	for _, e := range t.entries {
		if e.UID == uid && e.PublicKey == pubKey && e.Backend == backend {
			t.mu.RUnlock()
			return nil
		}
	}
	t.mu.RUnlock()
	key, err := computeMac1Key(pubKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	resolved, _ := t.resolve(ctx, backend)
	cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.OnChange != nil {
		t.OnChange(uid)
	}
	for i, e := range t.entries {
		if e.UID == uid {
			t.entries[i] = TableEntry{UID: uid, PublicKey: pubKey, Mac1Key: key, Backend: backend, Resolved: resolved}
			return nil
		}
	}
	t.entries = append(t.entries, TableEntry{UID: uid, PublicKey: pubKey, Mac1Key: key, Backend: backend, Resolved: resolved})
	return nil
}

// Entries is a copy of the table.
func (t *Table) Entries() []TableEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]TableEntry(nil), t.entries...)
}

// Delete removes the entry for uid.
func (t *Table) Delete(uid string) {
	t.updates.Lock()
	defer t.updates.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, e := range t.entries {
		if e.UID == uid {
			if t.OnChange != nil {
				t.OnChange(uid)
			}
			t.entries = append(t.entries[:i], t.entries[i+1:]...)
			return
		}
	}
}

// FindByMac1 brute-forces mac1 verification.
// packet is the raw WireGuard type-1 packet (UDP payload).
// mac1 occupies bytes [len-32 : len-16]; the message body for MAC is packet[:len-32].
// Returns the matched group UID and its VPN pod address.
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

// Reserve the matched group and conntrack identity under the same table read
// lock. A concurrent table retirement cannot leave a late old reservation.
func (t *Table) reserve(packet []byte, ci uint32, source Socket, ct *ConnTrack) (*net.UDPAddr, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	body, mac := packet[:len(packet)-32], packet[len(packet)-32:len(packet)-16]
	for _, e := range t.entries {
		computed := computeMAC(e.Mac1Key[:], body)
		if bytesEqual(computed[:], mac) {
			if e.Resolved == nil {
				return nil, false
			}
			return e.Resolved, ct.AddPartial(ci, source, socketOf(e.Resolved), e.UID)
		}
	}
	return nil, false
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
	old := w.uidOfs[name]
	if old != "" && old != uid {
		w.Table.Delete(old)
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
	if pubKey == "" || !lg.Status.VPN.Registered || !lg.VPNEnabled() {
		// VPN not yet ready — remove stale entry if present.
		w.Table.Delete(string(lg.UID))
		w.forget(lg.Name)
		return ctrl.Result{}, nil
	}

	// The VPN pod address is the one the operator published; nothing is looked up. No address, or one that is not a unicast IPv4 address on the
	// WireGuard port, drops the group's entry: its handshakes are dropped until the status is fresh. The operator rewrites it on every change of
	// the VPN pod, and the change reaches this reconcile as a status event.
	backend := lg.Status.VPN.Backend
	if !w.validBackend(backend) {
		if backend != "" {
			log.Info("the group's VPN address is not usable, dropping its entry", "group", lg.Name)
		}
		w.Table.Delete(string(lg.UID))
		w.forget(lg.Name)
		return ctrl.Result{}, nil
	}
	log.Info("updating demux table", "group", lg.Name, "backend", backend)
	if err := w.Table.Update(string(lg.UID), pubKey, backend); err != nil {
		return ctrl.Result{}, err
	}
	w.remember(lg.Name, string(lg.UID))
	return ctrl.Result{}, nil
}

// validBackend says the status address is a pod address the demux may send to: an IPv4 unicast address that is not the node's own loopback or a
// link-local one, and the WireGuard port of the group's VPN (nothing else is ever a backend, whatever the status holds).
func (w *LabGroupWatcher) validBackend(backend string) bool {
	host, port, err := net.SplitHostPort(backend)
	if err != nil {
		return false
	}
	if p, err := strconv.Atoi(port); err != nil || p != w.VPNServicePort {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

func (w *LabGroupWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.LabGroup{}, builder.WithPredicates(predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
			old, a := e.ObjectOld.(*laboratoryv1alpha1.LabGroup)
			next, b := e.ObjectNew.(*laboratoryv1alpha1.LabGroup)
			return !a || !b || routingInputsChanged(old, next)
		}})).
		Complete(reconcileutil.Quiet(w))
}
