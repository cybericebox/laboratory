package checker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The keepalive of a checker instance: one Lease per node (not one signal per check), renewed every KeepaliveInterval. Silence of a
// check then means "no change" while the Lease is fresh, and "the checker is dead" when it is not. The management agent can read
// the Leases (label LabelChecker) and report them; today nothing reads them.
const (
	// LabelChecker marks the Leases of the checkers.
	LabelChecker = "laboratory.cybericebox.com/checker"
	// AnnotationState is "ready" or "unavailable: <CODE>: <detail>" (the start-up check of the node, like the node annotations of
	// the node-agent). AnnotationVersion is the image tag the checker runs.
	AnnotationState   = "laboratory.cybericebox.com/checker-state"
	AnnotationVersion = "laboratory.cybericebox.com/checker-version"

	KeepaliveInterval = 20 * time.Second
	// LeaseDuration is how long a Lease counts as fresh after a renewal (three intervals).
	LeaseDuration = 60 * time.Second

	leasePrefix = "laboratory-checker-"
)

// LeaseName is the Lease of the checker of a node.
func LeaseName(node string) string {
	name := leasePrefix + node
	if len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(node))
	return leasePrefix + hex.EncodeToString(sum[:16])
}

// Keepalive renews the Lease of this node's checker.
type Keepalive struct {
	Client    client.Client
	Namespace string
	Node      string
	// Holder is the name of the pod.
	Holder  string
	Version string
	// State returns the current state text (see AnnotationState).
	State func() string
	Now   func() time.Time
}

// Beat renews the Lease once (server-side apply under one manager; the Lease is created by the first apply).
func (k *Keepalive) Beat(ctx context.Context) error {
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	lease := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata": map[string]any{
			"name": LeaseName(k.Node), "namespace": k.Namespace,
			"labels":      map[string]any{LabelChecker: "true"},
			"annotations": map[string]any{AnnotationState: k.State(), AnnotationVersion: k.Version},
		},
		"spec": map[string]any{
			"holderIdentity":       k.Holder,
			"leaseDurationSeconds": int64(LeaseDuration / time.Second),
			"renewTime":            now().UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		},
	}}
	if err := k.Client.Patch(ctx, lease, client.Apply, client.FieldOwner("lab-checker-keepalive"), client.ForceOwnership); err != nil {
		return fmt.Errorf("renew the lease of the checker of node %s: %w", k.Node, err)
	}
	return nil
}

// Run renews until ctx ends. A failed renewal is retried at the next interval (the Lease lasts three).
func (k *Keepalive) Run(ctx context.Context, onError func(error)) {
	t := time.NewTicker(KeepaliveInterval)
	defer t.Stop()
	for {
		if err := k.Beat(ctx); err != nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
