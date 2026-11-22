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
	// MaxLabDevices is the hard ceiling of the devices of one lab, every type counted (container, switch, hub); the CRD carries the same
	// number. The chart's limits.lab.maxDevices (default 32) can only lower it, and the chart refuses a larger value.
	MaxLabDevices = 64
	// MaxContainerInterfaces is the most interfaces of a container device (a switch or hub has its fixed 48 ports): one device cannot open
	// thousands of direct connections.
	MaxContainerInterfaces = 16
	MaxSwitchPorts         = 48
	// MaxConsolePorts and MaxSerialPorts are the most console ports (console<n>, aux<n>) and serial ports (serial<n>) of one container
	// device; the CRD carries the same numbers. A connector device needs one serial port per cable, so the serial cap is the NIC cap.
	MaxConsolePorts = 4
	MaxSerialPorts  = 16
	// MaxConsoleCablesPerLab is the most console cables of one lab: the device ceiling (the CRD carries the same number).
	MaxConsoleCablesPerLab = MaxLabDevices
	// ConsoleLineMaxBytesPerSecond caps one console line per direction (a real 115200-baud line is about 11 KiB/s), so a cable is never
	// a bulk path through a node.
	ConsoleLineMaxBytesPerSecond = 64 << 10
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

// ReservationOf is the name of the Reservation an object runs under (its reservation label); labelled says the label is there
// at all. An object without the label is unreserved; one with an empty label names no reservation, which is invalid (the
// scheduler never admits it, and it is not unreserved work either).
func ReservationOf(labels map[string]string) (name string, labelled bool) {
	name, labelled = labels[LabelReservation]
	return name, labelled
}

// TenantsNamespace holds the per-tenant Secrets: the enrollment token and the registry credentials of a tenant (images.pullSecret).
const TenantsNamespace = "laboratory-tenants"

// AccessKeysNamespace holds the access public keys of the tenants, one Secret each (AccessKeysSecret). The L7 proxy reads this namespace
// and no other: the tenants namespace holds enrollment tokens and registry credentials, which an internet-facing component must not be
// able to read.
const AccessKeysNamespace = "laboratory-access-keys"

// AnnotationRegenerateEnrollment on a Tenant asks the operator for a new enrollment token; the
// operator removes it once done.
const AnnotationRegenerateEnrollment = LabelPrefix + "regenerate-enrollment-token"

// AccessKeysConfigMap is the ConfigMap, in the namespace of the device access service, that mirrors the access public keys of every tenant
// (public data): one entry per key, named <tenant>.<key id>, the PEM public key as its value. The operator writes it from the Secrets of
// AccessKeysNamespace; the service reads it as files and verifies console grants with it, so it holds no key of its own and needs no API
// access.
const AccessKeysConfigMap = "laboratory-access-keys"

// AccessKeysConfigMapKey is the entry of AccessKeysConfigMap for a tenant's key. A tenant name has no dot, so the first dot splits it.
func AccessKeysConfigMapKey(tenant, keyID string) string { return tenant + "." + keyID }

// AccessKeysSecretTenant is the inverse of AccessKeysSecret: the tenant of a Secret name of AccessKeysNamespace, and false for a name that
// is not one.
func AccessKeysSecretTenant(secret string) (string, bool) {
	const prefix, suffix = "tenant-", "-access-keys"
	if len(secret) <= len(prefix)+len(suffix) || secret[:len(prefix)] != prefix || secret[len(secret)-len(suffix):] != suffix {
		return "", false
	}
	return secret[len(prefix) : len(secret)-len(suffix)], true
}

// AccessKeysSecret is the Secret (in AccessKeysNamespace) that holds the access public keys of a
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
