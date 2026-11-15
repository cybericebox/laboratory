package laboratory

import (
	"context"
	goerrors "errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// internetDeniedError is the refusal of a lab in which a device with internet: deny shares a broadcast domain with an Internet gateway.
type internetDeniedError struct {
	conflicts []laboratoryv1alpha1.InternetConflict
}

func (e *internetDeniedError) Error() string {
	c := e.conflicts[0]
	return fmt.Sprintf("InternetDeniedDeviceOnInternetNetwork: device %q has internet: deny but shares a broadcast domain with the Internet gateway %q", c.Device, c.Gateway)
}

// cutInternetDenied fails closed. A refused lab keeps the data plane of its last good spec, and a spec update may have turned a device
// that is on the Internet gateway's network into a denied one (or wired it there). The Connections that attach a conflicting device are
// deleted, so the device has no path to the gateway whatever the old wiring was; its other devices and networks are not touched.
func (r *LabReconciler) cutInternetDenied(ctx context.Context, lab *laboratoryv1alpha1.Lab, conflicts []laboratoryv1alpha1.InternetConflict) error {
	cut := map[string]bool{}
	for _, c := range conflicts {
		cut[c.Device] = true
	}
	var list laboratoryv1alpha1.ConnectionList
	if err := r.List(ctx, &list, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		c := &list.Items[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ep := range c.Spec.Endpoints {
			if cut[ep.Device] {
				if err := r.deleteConnection(ctx, c); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// failClosedOnInternetDenied cuts the denied devices off when the refusal of the lab is an internet conflict; any other error is left alone.
func (r *LabReconciler) failClosedOnInternetDenied(ctx context.Context, lab *laboratoryv1alpha1.Lab, validationErr error) error {
	var denied *internetDeniedError
	if !goerrors.As(validationErr, &denied) {
		return nil
	}
	return r.cutInternetDenied(ctx, lab, denied.conflicts)
}
