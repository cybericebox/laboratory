package labdhcp

import (
	"context"
	"fmt"
	"reflect"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func InputsChanged(old, next *lab.Lab, network string) bool {
	if old == nil || next == nil {
		return true
	}
	if !reflect.DeepEqual(old.Spec.Lifecycle, next.Spec.Lifecycle) || !reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp) {
		return true
	}
	if network == networkVPN {
		return !reflect.DeepEqual(old.Spec.VPN, next.Spec.VPN)
	}
	return !reflect.DeepEqual(old.Spec.Internet, next.Spec.Internet)
}
func poolPrefix(network string) string {
	if network == networkVPN {
		return "dhcp-vpn"
	}
	return "dhcp-inet"
}
func PoolLabName(pool *allocation.Pool, network string) string {
	for _, owner := range pool.OwnerReferences {
		if owner.Kind == "Lab" && owner.APIVersion == "laboratory.cybericebox.com/v1alpha1" && pool.Name == fmt.Sprintf("%s-%s-0", poolPrefix(network), owner.Name) {
			return owner.Name
		}
	}
	return ""
}

// Desired distinguishes an absent optional pool from a failed API read. A pool
// must belong to this exact Lab incarnation; its bitmap is not a lease ledger.
func Desired(ctx context.Context, c client.Reader, namespace, name, network string) (bool, []dhcp.Range, string, error) {
	var l lab.Lab
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &l); err != nil {
		return false, nil, "", fmt.Errorf("load lab DHCP settings: %w", err)
	}
	var pool allocation.Pool
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-%s-0", poolPrefix(network), name)}, &pool)
	if apierrors.IsNotFound(err) {
		return false, nil, "", nil
	}
	if err != nil {
		return false, nil, "", fmt.Errorf("load DHCP pool: %w", err)
	}
	if PoolLabName(&pool, network) != name {
		return false, nil, "", fmt.Errorf("DHCP pool ownership mismatch")
	}
	owned := false
	for _, owner := range pool.OwnerReferences {
		if owner.Kind == "Lab" && owner.Name == name && owner.UID == l.UID {
			owned = true
		}
	}
	if !owned {
		return false, nil, "", fmt.Errorf("DHCP pool lab incarnation mismatch")
	}
	segment := l.Spec.Internet
	if network == networkVPN {
		segment = l.Spec.VPN
	}
	if !pool.DeletionTimestamp.IsZero() || !l.DeletionTimestamp.IsZero() || l.Spec.Lifecycle.IsStopped() || !segment.Enabled || segment.DHCPServer == nil || !segment.DHCPServer.Enabled {
		return false, nil, "", nil
	}
	ranges, dns, err := Settings(&l, network)
	return err == nil, ranges, dns, err
}
