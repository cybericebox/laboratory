package names

import (
	"crypto/sha256"
	"encoding/hex"
)

// UpstreamNamespace holds the credentials of the upstream registries that the registry gate (and only the gate) reads: the tenant's,
// the platform's and a lab's own. No pod references them and no node can read them.
const UpstreamNamespace = "laboratory-upstream"

// UpstreamPlatformSecret is the Secret of the platform's own pull credentials (the operator's imagePullSecrets, re-keyed).
const UpstreamPlatformSecret = "platform"

// UpstreamTenantSecret is the Secret with a tenant's image pull credentials (Tenant.spec.images.pullSecret), written by the operator.
func UpstreamTenantSecret(tenant string) string { return "tenant-" + tenant }

// UpstreamLabSecret is the Secret with the credentials one lab brought (the RegistryCredential of the variant call), written by the agent:
// "lab-" and the first 16 hex characters of the sha256 of "<namespace>/<lab>".
func UpstreamLabSecret(namespace, lab string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + lab))
	return "lab-" + hex.EncodeToString(sum[:])[:16]
}

// The annotations of a lab's Secret in laboratory-upstream that say whose it is (its name is a hash): the operator's sweep reads them to
// remove the Secret of a lab that no longer exists.
const (
	AnnotationUpstreamLabNamespace = "laboratory.cybericebox.com/lab-namespace"
	AnnotationUpstreamLab          = "laboratory.cybericebox.com/lab"
)
