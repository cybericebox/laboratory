package names

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// MaxIDLen is the longest client-supplied object id (LabGroup, Lab, LabGroupClient,
	// deploy group).
	MaxIDLen = 64
	// MaxLabDevices is the hard ceiling of the devices of one lab (switches and hubs included); the CRD carries the same
	// number. The chart's limits.lab.maxDevices can only lower it.
	MaxLabDevices = 64
	// MaxDeployAfter is the most deploy_after keys of one object.
	MaxDeployAfter = 32

	// AnnotationID holds the id a client gave an object: the CR name is derived from
	// it (EncodeName) and every answer returns the original.
	AnnotationID = LabelPrefix + "id"
	// AnnotationDeployGroup holds the original deploy group (the label holds DeployKey of it).
	AnnotationDeployGroup = LabelPrefix + "deploy-group"
	// AnnotationIDMap is JSON {encoded: original} of the object names a policy refers to
	// whose CR name differs from the id.
	AnnotationIDMap = LabelPrefix + "id-map"
)

// ValidateID checks a client-supplied id: not empty, at most MaxIDLen bytes, no control
// characters.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("id is empty")
	}
	if len(id) > MaxIDLen {
		return fmt.Errorf("id %q is longer than %d characters", id, MaxIDLen)
	}
	if strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("id %q has control characters", id)
	}
	return nil
}

func hashKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "h" + new(big.Int).SetBytes(sum[:]).Text(36)
}

// EncodeName is the CR name of an object id: the id itself when it already is a valid
// DNS-1123 label (lowercase, at most 63 characters; every UUID is), otherwise "h" and the
// base36 of its SHA-256 (51 characters).
func EncodeName(id string) string {
	if len(id) <= 63 && len(validation.IsDNS1123Label(id)) == 0 {
		return id
	}
	return hashKey(id)
}

// DeployKey is the label value of a deploy group (or a deploy_after entry): the key itself
// when it is a valid label value of at most 63 characters, otherwise "h" and the base36 of
// its SHA-256. Empty stays empty. The operator maps deploy-after annotations with it too.
func DeployKey(key string) string {
	if key == "" || (len(key) <= 63 && len(validation.IsValidLabelValue(key)) == 0) {
		return key
	}
	return hashKey(key)
}

// IDOf is the id of an object: the original from the annotation, or the CR name of an
// object that was not made by the agent.
func IDOf(obj interface{ GetName() string }) string {
	if a, err := meta.Accessor(obj); err == nil {
		if id := a.GetAnnotations()[AnnotationID]; id != "" {
			return id
		}
	}
	return obj.GetName()
}

// DefaultTenant is the tenant of a caller without a client certificate (TLS or mTLS off) and
// of objects that carry no tenant label.
const DefaultTenant = "default"

// LabelTenant is the reserved label that stamps every object the management agent creates
// with the tenant of its caller (the client certificate CN). It is hidden from the API.
const LabelTenant = LabelPrefix + "tenant"

// TenantKey is the label value of a tenant: the certificate CN when it is a valid label value
// of at most 63 characters, otherwise "h" + base36(sha256). An empty CN is the default tenant.
func TenantKey(cn string) string {
	if cn == "" {
		return DefaultTenant
	}
	return DeployKey(cn)
}

// TenantOf is the tenant of an object: its tenant label, or the default tenant for an object
// created before tenancy.
func TenantOf(labels map[string]string) string {
	if t := labels[LabelTenant]; t != "" {
		return t
	}
	return DefaultTenant
}

// TenantsNamespace holds the per-tenant Secrets: the enrollment token and the access public keys.
const TenantsNamespace = "laboratory-tenants"

// ImagesNamespace holds what the node-agents read to pull images for the scheduler: the
// credentials of an ImagePull request. The node-agent's role covers this namespace only.
const ImagesNamespace = "laboratory-images"

// TenantPullSecret is the name of the Secret, in every group namespace of a tenant whose spec
// has images.pullSecret, that holds the tenant's registry credentials (a copy of the Secret
// of TenantsNamespace). Device pods reference it; the platform's own pull secrets are
// never given to them.
const TenantPullSecret = "tenant-registry"

// AnnotationRegenerateEnrollment on a Tenant asks the operator for a new enrollment token; the
// operator removes it once done.
const AnnotationRegenerateEnrollment = LabelPrefix + "regenerate-enrollment-token"

// AccessKeysSecret is the Secret (in TenantsNamespace) that holds the access public keys of a
// tenant, one entry per key id with the PEM public key. The proxy looks keys up by this name.
func AccessKeysSecret(tenant string) string { return "tenant-" + tenant + "-access-keys" }

// EnrollmentSecret is the Secret (in TenantsNamespace) that shows the tenant's enrollment
// token (key "token") to the admin until it is used.
func EnrollmentSecret(tenant string) string { return "tenant-" + tenant + "-enrollment" }

// LabelEnrollmentToken is the label the operator puts on a Tenant with the start of the hash of its unused enrollment token
// (EnrollmentTokenLabel), so the agent finds the Tenant of a token with one selector instead of reading every Tenant.
const LabelEnrollmentToken = LabelPrefix + "enrollment-token"

// EnrollmentTokenLabel is the label value for a token hash (hex SHA-256): its first 48 characters, which is plenty to
// narrow the list to one Tenant. The full hash is still compared.
func EnrollmentTokenLabel(hash string) string {
	if len(hash) > 48 {
		return hash[:48]
	}
	return hash
}

// EnrollmentTokenKey is the data key of the token in the enrollment Secret.
const EnrollmentTokenKey = "token"
