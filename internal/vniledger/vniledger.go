// Package vniledger is the record, per node, of the VNIs the node has flows for. The node-agent writes a number into its ConfigMap
// before it programs the first flow of that VNI and takes it out after the flows are confirmed gone; the operator does not give a VNI
// back to the pool while any node's ledger still has it, whether or not the Lab that owned it still exists. A ledger of a node that is
// no longer in the cluster is never dropped by itself: its VNIs stay held until someone settles it by hand.
package vniledger

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Label marks the ledger ConfigMaps; NodeAnnotation says whose it is (a node name can be longer than a label value).
	Label          = "laboratory.cybericebox.com/vni-ledger"
	NodeAnnotation = "laboratory.cybericebox.com/node"
	// NamePrefix + the node name is the name of the ConfigMap; the node-agent may write that one only (admission policy).
	NamePrefix = "vni-ledger-"

	callTimeout = 15 * time.Second
)

// ConfigMapName is the ledger of a node.
func ConfigMapName(node string) string { return NamePrefix + node }

// Ledger is the node side. Note and Clear are safe for concurrent use and persist before they return.
type Ledger struct {
	Client    client.Client
	Reader    client.Reader
	Namespace string
	Node      string

	mu     sync.Mutex
	loaded bool
	set    map[uint]bool
}

func (l *Ledger) key(vni uint) string { return strconv.FormatUint(uint64(vni), 10) }

func (l *Ledger) loadLocked(ctx context.Context) error {
	var cm corev1.ConfigMap
	err := l.Reader.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: ConfigMapName(l.Node)}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(l.Node), Namespace: l.Namespace,
			Labels: map[string]string{Label: "true"}, Annotations: map[string]string{NodeAnnotation: l.Node}}}
		if err := l.Client.Create(ctx, &cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := l.Reader.Get(ctx, client.ObjectKeyFromObject(&cm), &cm); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	set := map[uint]bool{}
	for k := range cm.Data {
		n, perr := strconv.ParseUint(k, 10, 32)
		if perr != nil {
			return fmt.Errorf("VNI ledger of the node has a key that is not a number: %q", k)
		}
		set[uint(n)] = true
	}
	l.set, l.loaded = set, true
	return nil
}

// Load reads (and, on a fresh node, creates) the ledger. Note and Clear load it on first use, so calling Load is only to fail early.
func (l *Ledger) Load(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loaded {
		return nil
	}
	return l.loadLocked(ctx)
}

// Note records that the node has (or is about to have) flows of the VNI. It returns only after the record is stored: the flows are
// programmed after, and an error means they must not be.
func (l *Ledger) Note(vni uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		if err := l.loadLocked(ctx); err != nil {
			return err
		}
	}
	if l.set[vni] {
		return nil
	}
	patch := fmt.Sprintf(`{"data":{%q:%q}}`, l.key(vni), time.Now().UTC().Format(time.RFC3339))
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(l.Node), Namespace: l.Namespace}}
	if err := l.Client.Patch(ctx, cm, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return err
	}
	l.set[vni] = true
	return nil
}

// Clear takes the VNI out of the ledger, after its flows are confirmed gone.
func (l *Ledger) Clear(vni uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		if err := l.loadLocked(ctx); err != nil {
			return err
		}
	}
	if !l.set[vni] {
		return nil
	}
	patch := fmt.Sprintf(`{"data":{%q:null}}`, l.key(vni))
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(l.Node), Namespace: l.Namespace}}
	if err := l.Client.Patch(ctx, cm, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	delete(l.set, vni)
	return nil
}

// Entries are the VNIs the ledger holds now, sorted.
func (l *Ledger) Entries(ctx context.Context) ([]uint, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		if err := l.loadLocked(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]uint, 0, len(l.set))
	for v := range l.set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Snapshot is every ledger of the cluster, read by the operator.
type Snapshot struct {
	// Held says, per VNI, the nodes whose ledger has it.
	Held map[uint][]string
	// Nodes are the nodes that have a ledger with at least one entry.
	Nodes []string
}

// Read lists the ledgers. A key that is not a number fails the read (fail-closed: nothing is returned to the pool on a record that
// cannot be understood).
func Read(ctx context.Context, r client.Reader, namespace string) (*Snapshot, error) {
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms, client.InNamespace(namespace), client.MatchingLabels{Label: "true"}); err != nil {
		return nil, err
	}
	s := &Snapshot{Held: map[uint][]string{}}
	for i := range cms.Items {
		cm := &cms.Items[i]
		node := cm.Annotations[NodeAnnotation]
		if node == "" {
			node = strings.TrimPrefix(cm.Name, NamePrefix)
		}
		if len(cm.Data) > 0 {
			s.Nodes = append(s.Nodes, node)
		}
		for k := range cm.Data {
			n, err := strconv.ParseUint(k, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("VNI ledger %s has a key that is not a number: %q", cm.Name, k)
			}
			s.Held[uint(n)] = append(s.Held[uint(n)], node)
		}
	}
	return s, nil
}
