//go:build linux

package reconciler

import (
	"fmt"
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// buildLabAccessSnapshots makes one snapshot per VPN gateway of every lab, keyed by the name of the gateway's LabVPN (unique per gateway).
// A gateway that has no leg yet has no snapshot.
func buildLabAccessSnapshots(labs []lab.Lab, legs []lab.LabVPN) (map[string]vpn.LabAccessSnapshot, error) {
	byName := make(map[string]*lab.Lab, len(labs))
	for i := range labs {
		byName[labs[i].Name] = &labs[i]
	}
	result := make(map[string]vpn.LabAccessSnapshot, len(legs))
	interfaces := map[string]string{}
	for i := range legs {
		leg := &legs[i]
		if !leg.DeletionTimestamp.IsZero() {
			continue
		}
		l, ok := byName[leg.Spec.LabName]
		if !ok {
			continue
		}
		g, ok := l.Spec.Gateway(leg.Spec.Device)
		if !ok || g.Kind != lab.GatewayKindVPN {
			continue
		}
		iface := names.LabIfaceNameByIndex(leg.Spec.NetworkIndex)
		if _, err := vpn.LabInterfaceIndex(iface); err != nil {
			return nil, err
		}
		if owner, ok := interfaces[iface]; ok {
			return nil, fmt.Errorf("VPN interface %s belongs to both %s and %s", iface, owner, leg.Name)
		}
		interfaces[iface] = leg.Name
		result[leg.Name] = vpn.LabAccessSnapshot{
			LabName: l.Name, Gateway: g.Name, VPNCIDR: l.Status.GatewayCIDR(g.Name),
			Ready: labAccessReady(l, g.Name), LabInitiated: g.ReverseConnections, Interface: iface,
		}
	}
	return result, nil
}

func labVPNAccessChanges() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, oldOK := e.ObjectOld.(*lab.LabVPN)
		next, nextOK := e.ObjectNew.(*lab.LabVPN)
		return !oldOK || !nextOK || old == nil || next == nil || old.Spec != next.Spec ||
			!reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp)
	}}
}
