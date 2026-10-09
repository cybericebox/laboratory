package demux

import (
	"reflect"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func routingInputsChanged(old, next *lab.LabGroup) bool {
	return old == nil || next == nil || old.UID != next.UID || !reflect.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp) || lab.LabGroupNamespaceOf(old) != lab.LabGroupNamespaceOf(next) || old.Status.VPN.PublicKey != next.Status.VPN.PublicKey || old.Status.VPN.Registered != next.Status.VPN.Registered || old.Spec.VPN.Disabled != next.Spec.VPN.Disabled
}
