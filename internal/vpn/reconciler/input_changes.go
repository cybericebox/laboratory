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
	return old == nil || next == nil || old.Status.VPN.CIDR != next.Status.VPN.CIDR ||
		labAccessReady(old) != labAccessReady(next) ||
		!reflect.DeepEqual(old.Spec.Lifecycle, next.Spec.Lifecycle) ||
		!reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp)
}

func labAccessReady(l *lab.Lab) bool {
	return l.DeletionTimestamp.IsZero() && !l.Spec.Lifecycle.IsStopped() && l.Status.Phase == lab.PhaseReady && l.Status.VPN.Ready
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
