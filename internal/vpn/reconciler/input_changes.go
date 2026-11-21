//go:build linux

package reconciler

import (
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func clientPeerInputsChanged(old, next *lab.LabGroupClient) bool {
	return clientAccessInputsChanged(old, next) || old.Spec.PublicKey != next.Spec.PublicKey
}

func clientAccessInputsChanged(old, next *lab.LabGroupClient) bool {
	return old == nil || next == nil || old.Status.AssignedIP != next.Status.AssignedIP ||
		!reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp)
}

func labAccessInputsChanged(old, next *lab.Lab) bool {
	return old == nil || next == nil || !reflect.DeepEqual(accessGatewayInputs(old), accessGatewayInputs(next)) ||
		old.Status.Phase != next.Status.Phase ||
		!reflect.DeepEqual(old.Spec.Lifecycle, next.Spec.Lifecycle) ||
		!reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp)
}

// accessGateway is what the access rules of a VPN gateway depend on.
type accessGateway struct {
	Name         string
	CIDR         string
	Reverse      bool
	Ready        bool
	NetworkIndex uint
}

// accessGatewayInputs lists, in spec order, what the access rules depend on for every VPN gateway of the lab.
func accessGatewayInputs(l *lab.Lab) []accessGateway {
	vpns := l.Spec.GatewaysOfKind(lab.GatewayKindVPN)
	out := make([]accessGateway, 0, len(vpns))
	for _, g := range vpns {
		in := accessGateway{Name: g.Name, Reverse: g.ReverseConnections, Ready: labAccessReady(l, g.Name)}
		if st := l.Status.GatewayStatus(g.Name); st != nil {
			in.CIDR, in.NetworkIndex = st.CIDR, st.NetworkIndex
		}
		out = append(out, in)
	}
	return out
}

// labAccessReady says clients may be admitted to the VPN gateway of the lab: the lab runs and the gateway is ready.
func labAccessReady(l *lab.Lab, gateway string) bool {
	st := l.Status.GatewayStatus(gateway)
	return l.DeletionTimestamp.IsZero() && !l.Spec.Lifecycle.IsStopped() && l.Status.Phase == lab.PhaseReady && st != nil && st.Ready
}

func clientChanges(compare func(*lab.LabGroupClient, *lab.LabGroupClient) bool) predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, oldOK := e.ObjectOld.(*lab.LabGroupClient)
		next, nextOK := e.ObjectNew.(*lab.LabGroupClient)
		return !oldOK || !nextOK || compare(old, next)
	}}
}

func clientPeerChanges() predicate.Predicate   { return clientChanges(clientPeerInputsChanged) }
func clientAccessChanges() predicate.Predicate { return clientChanges(clientAccessInputsChanged) }

func labAccessChanges() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		old, oldOK := e.ObjectOld.(*lab.Lab)
		next, nextOK := e.ObjectNew.(*lab.Lab)
		return !oldOK || !nextOK || labAccessInputsChanged(old, next)
	}}
}
