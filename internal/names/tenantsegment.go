package names

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// SegmentPlatform is the tenant segment of the components of the platform itself (the VPN and gateway pods of every group): their
// images go through the cache too, with the credentials of the Secret "platform".
const SegmentPlatform = "platform"

var (
	reSafeSegment = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reHashSegment = regexp.MustCompile(`^t-[0-9a-f]{12}$`)
)

// TenantSegment is the path component that carries a tenant in an image reference: <prefix>/c/<segment>/<registry>/<repo>. The operator
// writes it and the registry gate maps it back with its own list of tenants; the gate never decodes it.
//
// A name that is safe as an OCI path component (lowercase letters, digits and hyphens, at most 63 characters, no leading or trailing
// hyphen: nothing that could look like a registry host) is used as it is, unless it is reserved: "platform" and anything of the form
// t-<12 hex> are encoded like every other name that is not safe, as t-<first 12 hex characters of the sha256 of the name>.
func TenantSegment(name string) string {
	if reSafeSegment.MatchString(name) && name != SegmentPlatform && !reHashSegment.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "t-" + hex.EncodeToString(sum[:])[:12]
}

// ValidSegment says whether s has the form of a tenant segment (a safe name, "platform" or t-<12 hex>): what the gate accepts as the first
// component after "c/" before it looks the segment up.
func ValidSegment(s string) bool { return reSafeSegment.MatchString(s) }
