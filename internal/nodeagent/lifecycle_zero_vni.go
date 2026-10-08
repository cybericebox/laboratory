//go:build linux

package nodeagent

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func completeZeroVNIBinding(b lab.OwnedVNI) bool {
	return b.UID != "" && b.OwnerUID != "" && b.PoolUID != "" && b.LeaseGeneration > 0 && b.Namespace != "" && b.Name != "" && (b.Kind == "Device" || b.Kind == "Connection")
}

// Numeric zero is valid only with the same actual Pool reservation and typed
// API owner used for positive indices. This never produces a release receipt.
func validateZeroVNIBinding(ctx context.Context, reader client.Reader, b lab.OwnedVNI) (client.Object, error) {
	if reader == nil || !completeZeroVNIBinding(b) {
		return nil, ErrPortOwnerUnknown
	}
	if err := poolpkg.ValidateLease(ctx, reader, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: b.VNI, PoolUID: b.PoolUID, OwnerUID: b.UID, Generation: b.LeaseGeneration}); err != nil {
		return nil, err
	}
	var object client.Object
	if b.Kind == "Device" {
		object = &lab.Device{}
	} else {
		object = &lab.Connection{}
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Name}, object); err != nil {
		return nil, err
	}
	if string(object.GetUID()) != b.UID {
		return nil, ErrPortOwnerChanged
	}
	var index *uint
	var lease *lab.VNILease
	var labName string
	switch current := object.(type) {
	case *lab.Device:
		index = current.Status.VNI
		lease = current.Status.VNILease
		labName = current.Spec.LabRef
	case *lab.Connection:
		index = current.Status.VNI
		lease = current.Status.VNILease
		labName = current.Spec.LabRef
	}
	if index == nil || *index != b.VNI || lease == nil || lease.PoolUID != b.PoolUID || lease.Generation != b.LeaseGeneration {
		return nil, ErrPortOwnerChanged
	}
	var parent lab.Lab
	if err := reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: labName}, &parent); err != nil {
		return nil, err
	}
	ref, ok := nativeOwnerReference(object.GetOwnerReferences(), "Lab")
	if !ok || ref.UID != parent.UID || ref.Name != parent.Name || b.OwnerUID != string(parent.UID) {
		return nil, ErrPortOwnerChanged
	}
	return object, nil
}

// The default-off caller supplies its direct reader, not a fabricated tuple.
// Both actual lease and exact API revision are checked across the native barrier.
func (r *ConnectionReconciler) zeroVNIRetirementAuthority() func(context.Context, lab.OwnedVNI) error {
	initialRV := ""
	return func(ctx context.Context, b lab.OwnedVNI) error {
		object, err := validateZeroVNIBinding(ctx, r.directReader(), b)
		if err != nil {
			return err
		}
		if initialRV != "" && object.GetResourceVersion() != initialRV {
			return ErrPortOwnerChanged
		}
		initialRV = object.GetResourceVersion()
		var parent lab.Lab
		var labName string
		switch current := object.(type) {
		case *lab.Device:
			labName = current.Spec.LabRef
		case *lab.Connection:
			labName = current.Spec.LabRef
		}
		if err := r.directReader().Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: labName}, &parent); err != nil {
			return err
		}
		op, rev := nativeLabOperation(&parent)
		if parent.Generation != b.Generation || op != b.OperationID || rev != b.Revision {
			return ErrPortOwnerChanged
		}
		return nil
	}
}
