package labdhcp

import (
	"context"
	"fmt"
	"reflect"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// InputsChanged says whether what the DHCP servers of a lab depend on changed: its gateways, its lifecycle, its deletion.
func InputsChanged(old, next *lab.Lab) bool {
	if old == nil || next == nil {
		return true
	}
	if !reflect.DeepEqual(old.Spec.Lifecycle, next.Spec.Lifecycle) || !reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp) {
		return true
	}
	return !reflect.DeepEqual(old.Spec.Gateways, next.Spec.Gateways)
}

// PoolOwner says which Lab and which of its gateways a DHCP pool is for, or "" and "" when the pool is not one made for a Lab.
func PoolOwner(pool *allocation.Pool) (labName, gateway string) {
	for _, owner := range pool.OwnerReferences {
		if owner.Kind != "Lab" || owner.APIVersion != "laboratory.cybericebox.com/v1alpha1" {
			continue
		}
		if g := names.DHCPPoolOwner(pool.Name, owner.Name); g != "" {
			return owner.Name, g
		}
	}
	return "", ""
}

// Desired distinguishes an absent optional pool from a failed API read. A pool must belong to this exact Lab incarnation; its bitmap is
// not a lease ledger.
func Desired(ctx context.Context, c client.Reader, namespace, labName, gateway string) (bool, []dhcp.Range, string, error) {
	var l lab.Lab
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: labName}, &l); err != nil {
		return false, nil, "", fmt.Errorf("load lab DHCP settings: %w", err)
	}
	var pool allocation.Pool
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: names.DHCPPoolName(labName, gateway)}, &pool)
	if apierrors.IsNotFound(err) {
		return false, nil, "", nil
	}
	if err != nil {
		return false, nil, "", fmt.Errorf("load DHCP pool: %w", err)
	}
	if owner, g := PoolOwner(&pool); owner != labName || g != gateway {
		return false, nil, "", fmt.Errorf("DHCP pool ownership mismatch")
	}
	owned := false
	for _, owner := range pool.OwnerReferences {
		if owner.Kind == "Lab" && owner.Name == labName && owner.UID == l.UID {
			owned = true
		}
	}
	if !owned {
		return false, nil, "", fmt.Errorf("DHCP pool lab incarnation mismatch")
	}
	g, ok := l.Spec.Gateway(gateway)
	if !pool.DeletionTimestamp.IsZero() || !l.DeletionTimestamp.IsZero() || l.Spec.Lifecycle.IsStopped() || !ok || g.DHCPServer == nil || !g.DHCPServer.Enabled {
		return false, nil, "", nil
	}
	ranges, dns, err := Settings(&l, gateway)
	return err == nil, ranges, dns, err
}
