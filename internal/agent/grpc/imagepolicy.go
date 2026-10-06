package grpc

import (
	"context"
	"fmt"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagepolicy"
)

// imagePolicy is what the caller's tenant may run: the platform deny list and cache
// addresses, narrowed by the tenant's allow list. The tenant is nil for a caller without a
// Tenant object (the default tenant of a development setup).
func (h *Handler) imagePolicy(ctx context.Context) (imagepolicy.Policy, *laboratoryv1alpha1.Tenant, error) {
	var ten *laboratoryv1alpha1.Tenant
	if h.cs != nil {
		var err error
		if ten, err = h.tenantObject(ctx, tenantOf(ctx)); err != nil {
			return imagepolicy.Policy{}, nil, err
		}
	}
	p := imagepolicy.Policy{Deny: h.imageDeny, CachePrefixes: h.imageCache}
	if ten != nil {
		p.Allow = ten.Spec.Images.Allow
	}
	return p, ten, nil
}

// checkSpecImages refuses a spec that names an image the tenant may not use.
func checkSpecImages(p imagepolicy.Policy, spec *laboratoryv1alpha1.LabSpec) error {
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Image == "" {
			continue
		}
		if err := p.Check(d.Image); err != nil {
			return fmt.Errorf("device %q: %w", d.Name, err)
		}
	}
	return nil
}
