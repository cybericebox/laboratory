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
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
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
	p := imagepolicy.Policy{Deny: h.imageDeny, CachePrefixes: h.imageCache, PlatformOnly: h.platformOnly, PlatformPrefix: h.platformPrefix}
	if p.PlatformPrefix == "" {
		p.PlatformPrefix = names.RegistryNodePrefix
	}
	if ten != nil {
		p.Allow = ten.Spec.Images.Allow
		p.PlatformOnly = p.PlatformOnly || ten.Spec.Images.PlatformOnly
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

// mapSpecImages turns the platform references of a spec that passed checkSpecImages into the references the nodes pull
// (localhost:<port>/platform/<name>@sha256:...). Nothing else is changed, and a platform image needs no pull credentials: the node's
// registry forwarder reads platform/** with the platform's reader account.
func mapSpecImages(p imagepolicy.Policy, spec *laboratoryv1alpha1.LabSpec) {
	for i := range spec.Devices {
		spec.Devices[i].Image = p.Map(spec.Devices[i].Image)
	}
}

// imageRefusal is the gRPC status of an image the policy refuses: PermissionDenied, and for an image that is not a platform image
// where only those run, the reason IMAGE_NOT_PLATFORM (the message names the variant, the device and the image).
func imageRefusal(prefix string, err error) error {
	st := status.New(codes.PermissionDenied, prefix+err.Error())
	if errors.Is(err, imagepolicy.ErrNotPlatform) {
		if withInfo, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: client.ReasonImageNotPlatform, Domain: client.ErrorDomain}); derr == nil {
			st = withInfo
		}
	}
	return st.Err()
}
