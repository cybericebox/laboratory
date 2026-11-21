package grpc

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagepolicy"
	"github.com/cybericebox/laboratory/pkg/agent/client"
)

// imagePolicy is what the caller's tenant may run: the platform deny list and cache
// addresses, narrowed by the tenant's allow list. The tenant is nil for a caller without a
// Tenant object (the default tenant of a development setup).
func (h *Handler) imagePolicy(ctx context.Context) (imagepolicy.Policy, error) {
	var ten *laboratoryv1alpha1.Tenant
	if h.cs != nil {
		var err error
		if ten, err = h.tenantObject(ctx, tenantOf(ctx)); err != nil {
			return imagepolicy.Policy{}, err
		}
	}
	p := imagepolicy.Policy{Deny: h.imageDeny, CachePrefixes: h.imageCache,
		Registries: h.allowedRegistries}
	if ten != nil {
		p.Allow = ten.Spec.Images.Allow
	}
	return p, nil
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

// imageRefusal is the gRPC status of an image the policy refuses: PermissionDenied, and for a platform: reference (no longer served) the
// reason IMAGE_NOT_PLATFORM (the message names the variant, the device and the image).
func imageRefusal(prefix string, err error) error {
	st := status.New(codes.PermissionDenied, prefix+err.Error())
	reason := ""
	switch {
	case errors.Is(err, imagepolicy.ErrNotPlatform):
		reason = client.ReasonImageNotPlatform
	case errors.Is(err, imagepolicy.ErrRegistryNotAllowed):
		reason = client.ReasonRegistryNotAllowed
	}
	if reason != "" {
		if withInfo, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: client.ErrorDomain}); derr == nil {
			st = withInfo
		}
	}
	return st.Err()
}
