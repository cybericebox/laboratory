package names

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	labShortIDLen = 6
	// maxDNSLabel is the DNS-1035 label limit that Service names and the first
	// label of a web host share.
	maxDNSLabel = 63
)

// LabShortID is a stable short identifier of a Lab inside its LabGroup. Lab
// names (c-<challenge id>[-g<N>]) are unique per group, so the digest is too
// for any practical purpose, and it changes with the generation of a recreated
// Lab.
func LabShortID(labName string) string {
	sum := sha256.Sum256([]byte(labName))
	return hex.EncodeToString(sum[:])[:labShortIDLen]
}

// WebHostLabel is the first DNS label of a web-exposed device, both as the
// name of its Service and as the host under the base domain:
// <device>-<labShortID>. Two labs of one group may both have a device called
// "web"; their hosts and Services never collide.
func WebHostLabel(labName, device string) string {
	if room := maxDNSLabel - 1 - labShortIDLen; len(device) > room {
		device = device[:room]
	}
	return device + "-" + LabShortID(labName)
}
