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

func buildLabAccessSnapshots(labs []lab.Lab, legs []lab.LabVPN) (map[string]vpn.LabAccessSnapshot, error) {
	result := make(map[string]vpn.LabAccessSnapshot, len(labs))
	for i := range labs {
		l := &labs[i]
		result[l.Name] = vpn.LabAccessSnapshot{VPNCIDR: l.Status.VPN.CIDR, Ready: labAccessReady(l)}
	}
	interfaces := map[string]string{}
	for i := range legs {
		leg := &legs[i]
		if !leg.DeletionTimestamp.IsZero() {
			continue
		}
		s, ok := result[leg.Spec.LabName]
		if !ok {
			continue
		}
		iface := names.LabIfaceNameByIndex(leg.Spec.NetworkIndex)
		if _, err := vpn.LabInterfaceIndex(iface); err != nil {
			return nil, err
		}
		if s.Interface != "" {
			return nil, fmt.Errorf("multiple VPN legs for lab %s", leg.Spec.LabName)
		}
		if owner, ok := interfaces[iface]; ok {
			return nil, fmt.Errorf("VPN interface %s belongs to both %s and %s", iface, owner, leg.Spec.LabName)
		}
		interfaces[iface] = leg.Spec.LabName
		s.Interface = iface
		result[leg.Spec.LabName] = s
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
