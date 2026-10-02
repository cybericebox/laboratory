package grpc

import (
	"context"
	"crypto/x509"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/tenant"
)

// tenantOf derives the tenant of a call from the verified client certificate CN. A call
// without a client certificate (TLS or mTLS off, local development) is the default tenant.
func tenantOf(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			return names.TenantKey(info.State.PeerCertificates[0].Subject.CommonName)
		}
	}
	return names.DefaultTenant
}

// stampTenant adds the tenant label to a label map (allocating it).
func stampTenant(labels map[string]string, tenant string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[names.LabelTenant] = tenant
	return labels
}

// ownedBy reports whether an object belongs to the tenant. An object without a tenant
// label (created before tenancy) belongs to the default tenant.
func ownedBy(tenant string, obj metav1.Object) bool {
	return names.TenantOf(obj.GetLabels()) == tenant
}

// notFoundForeign is what a tenant sees of another tenant's object: nothing.
func notFoundForeign(kind, name string) error {
	return apierrors.NewNotFound(laboratoryv1alpha1.Resource(kind), name)
}

// listGroups lists the LabGroups of the caller's tenant that match a (user) selector.
func (h *Handler) listGroups(ctx context.Context, selector string) ([]laboratoryv1alpha1.LabGroup, error) {
	tenant := tenantOf(ctx)
	if tenant != names.DefaultTenant {
		// Narrow on the server; the default tenant also owns unlabelled objects, so it filters here.
		if selector != "" {
			selector += ","
		}
		selector += names.LabelTenant + "=" + tenant
	}
	list, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for i := range list.Items {
		if ownedBy(tenant, &list.Items[i]) {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// getGroup reads a LabGroup by id; another tenant's group is NotFound.
func (h *Handler) getGroup(ctx context.Context, id string) (*laboratoryv1alpha1.LabGroup, error) {
	g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, crName(id), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !ownedBy(tenantOf(ctx), g) {
		return nil, notFoundForeign(kindLabGroup, id)
	}
	return g, nil
}

// errTaken is the answer to creating an id that belongs to another tenant: it must not
// tell that the object exists.
type errTaken struct{ kind, id string }

func (e errTaken) Error() string { return e.kind + " " + e.id + ": the id is not available" }

// persistenceAllowed: the platform allows persistence and the caller's tenant is permitted it.
func (h *Handler) persistenceAllowed(ctx context.Context) (bool, error) {
	ten, err := h.tenantObject(ctx, tenantOf(ctx))
	if err != nil {
		return false, err
	}
	return tenant.EffectivePersistence(ten, h.statePersistence, 0, 0).Allowed, nil
}

// Authorize admits a call: the caller's certificate must name a Tenant that exists (an unknown CN is PermissionDenied, the
// default tenant included: it has a Tenant object like any other, so its certificates can be revoked too) and must belong to the
// tenant's current enrollment epoch.
func (h *Handler) Authorize(ctx context.Context) error {
	cert := callerCert(ctx)
	if cert == nil {
		// No certificate in the context: mTLS is off, which the server allows only when it was started as insecure (every
		// caller is the default tenant). With mTLS on the server has refused such a call before it gets here.
		return nil
	}
	if cert.Subject.CommonName == "" {
		return status.Error(codes.Unauthenticated, "the client certificate has no common name")
	}
	// The handshake checked the validity once, when the connection opened; a long-lived stream asks again.
	if !cert.NotAfter.IsZero() && h.now().After(cert.NotAfter) {
		return status.Error(codes.Unauthenticated, "the client certificate has expired")
	}
	name := tenantOf(ctx)
	ten, err := h.tenantObject(ctx, name)
	if err != nil {
		return err
	}
	if ten == nil {
		return status.Errorf(codes.PermissionDenied, "client %q is not a tenant", name)
	}
	return checkEpoch(ctx, ten)
}

// checkEpoch refuses a client certificate that does not belong to the tenant's current enrollment epoch. A certificate carries
// the epoch number it was issued in and the UID of the Tenant; both must equal the Tenant's now, exactly. Enrolling again moves
// the number, which revokes every earlier certificate; a Tenant created again under the same name has another UID.
//
// A certificate without the claim (issued before the epoch was a number) is judged by time as before (issued before the
// Tenant's creation or before the last enrollment: refused), and refused outright once the epoch is above zero, that is after the
// first enrollment by this version: that closes the one-second hole of the time comparison for everything that enrolls again.
func checkEpoch(ctx context.Context, ten *laboratoryv1alpha1.Tenant) error {
	cert := callerCert(ctx)
	if cert == nil {
		return nil // no client certificate in this context: mTLS off (the server refuses that unless it is told to allow it)
	}
	revoked := status.Errorf(codes.PermissionDenied, "the client certificate of %q was issued before the tenant's current enrollment: enroll again", ten.Name)
	claim, has, err := certEpoch(cert)
	if err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	if has {
		if claim.Epoch != ten.Status.CertificateEpoch || claim.TenantUID != string(ten.UID) {
			return revoked
		}
		return nil
	}
	if ten.Status.CertificateEpoch > 0 {
		return revoked
	}
	floor := ten.CreationTimestamp.Time
	if nb := ten.Status.CertificatesNotBefore; nb != nil && nb.Time.After(floor) {
		floor = nb.Time
	}
	if certIssuedAt(cert).Truncate(time.Second).Before(floor.Truncate(time.Second)) {
		return revoked
	}
	return nil
}

// callerCert is the verified client certificate of the call, nil without one.
func callerCert(ctx context.Context) *x509.Certificate {
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			return info.State.PeerCertificates[0]
		}
	}
	return nil
}
