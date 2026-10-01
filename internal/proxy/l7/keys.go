package l7

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// SecretKeys looks the access public key of a tenant up in the Secret tenant-<name>-access-keys of
// the tenants namespace (one entry per key id, the PEM public key). The reader is a cache that
// watches that namespace, so a key added by the agent works at once and a removed one stops.
func SecretKeys(r client.Reader) KeyLookup {
	return func(tenant, keyID string) (ed25519.PublicKey, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: names.TenantsNamespace, Name: names.AccessKeysSecret(tenant)}, &s); err != nil {
			return nil, false
		}
		return ParseAccessKey(s.Data[keyID])
	}
}

// ParseAccessKey reads a PKIX PEM Ed25519 public key.
func ParseAccessKey(data []byte) (ed25519.PublicKey, bool) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, false
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, false
	}
	k, ok := pub.(ed25519.PublicKey)
	return k, ok
}

// LabGroupTenant tells the tenant of a LabGroup from its tenant label: the group's custom resource
// is named after the platform's id (encoded when needed). A group created before tenancy has no label
// and belongs to the default tenant.
func LabGroupTenant(r client.Reader) GroupTenant {
	return func(groupID string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var g laboratoryv1alpha1.LabGroup
		if err := r.Get(ctx, types.NamespacedName{Name: names.EncodeName(groupID)}, &g); err != nil {
			return "", false
		}
		return names.TenantOf(g.Labels), true
	}
}
