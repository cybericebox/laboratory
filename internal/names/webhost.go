package names

import (
	"fmt"
	"math/big"
	"strings"
)

const (
	// LabIDLen is the fixed width of a lab id: 36^25 > 2^128, so any UUID fits.
	LabIDLen = 25
	// maxDNSLabel is the DNS-1035 label limit that Service names and the first
	// label of a web host share.
	maxDNSLabel = 63

	// MaxDeviceNameLen is the longest device name a lab may have. A web-exposed
	// device is served at <device>-<labid>.<base domain>, and the wildcard cert
	// covers one label level, so <device>-<labid> must fit one DNS label:
	// maxDNSLabel - 1 (hyphen) - LabIDLen = 37. Longer names are rejected, never
	// truncated: truncation could make two devices collide.
	MaxDeviceNameLen = maxDNSLabel - 1 - LabIDLen

	base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
)

var (
	base36Radix = big.NewInt(36)
	maxUUID     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
)

// LabID encodes a lab's UUID as 25 lowercase base36 characters, left-padded
// with '0'. Base64 would be shorter, but DNS labels are case-insensitive and
// allow only [a-z0-9-]. The UUID string may have dashes or not.
func LabID(uuid string) (string, error) {
	hexed := strings.ReplaceAll(strings.ToLower(uuid), "-", "")
	if len(hexed) != 32 {
		return "", fmt.Errorf("names: %q is not a UUID", uuid)
	}
	n, ok := new(big.Int).SetString(hexed, 16)
	if !ok {
		return "", fmt.Errorf("names: %q is not a UUID", uuid)
	}
	out := make([]byte, LabIDLen)
	for i := LabIDLen - 1; i >= 0; i-- {
		var digit big.Int
		n.DivMod(n, base36Radix, &digit)
		out[i] = base36[digit.Int64()]
	}
	return string(out), nil
}

// ParseLabID decodes a lab id back to the canonical lowercase UUID string.
func ParseLabID(id string) (string, bool) {
	if len(id) != LabIDLen {
		return "", false
	}
	n := new(big.Int)
	for _, c := range id {
		d := strings.IndexRune(base36, c)
		if d < 0 {
			return "", false
		}
		n.Mul(n, base36Radix)
		n.Add(n, big.NewInt(int64(d)))
	}
	if n.Cmp(maxUUID) > 0 {
		return "", false
	}
	h := fmt.Sprintf("%032x", n)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], true
}

// ValidateDeviceName checks a device name against the DNS label rules that the
// web host label <device>-<labid> imposes: at most MaxDeviceNameLen characters
// of lowercase a-z, 0-9 and '-', not starting or ending with '-'.
func ValidateDeviceName(name string) error {
	if name == "" {
		return fmt.Errorf("device name is empty")
	}
	if len(name) > MaxDeviceNameLen {
		return fmt.Errorf("device name %q is %d characters, the limit is %d", name, len(name), MaxDeviceNameLen)
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("device name %q must not start or end with '-'", name)
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return fmt.Errorf("device name %q may contain only lowercase a-z, 0-9 and '-'", name)
		}
	}
	return nil
}

// WebHostLabel is the first DNS label of a web-exposed device, both as the name
// of its Service and as the host under the base domain: <device>-<labid>. Two
// labs of one group may both have a device called "web"; their hosts and
// Services never collide. The device name must already satisfy
// ValidateDeviceName; it is not truncated here.
func WebHostLabel(labUID, device string) string {
	id, err := LabID(labUID)
	if err != nil {
		id = strings.Repeat("0", LabIDLen)
	}
	return device + "-" + id
}

// LabIDFromWebHostLabel extracts the lab id from a <device>-<labid> label.
func LabIDFromWebHostLabel(label string) (string, bool) {
	if len(label) < LabIDLen+2 || label[len(label)-LabIDLen-1] != '-' {
		return "", false
	}
	id := label[len(label)-LabIDLen:]
	_, ok := ParseLabID(id)
	return id, ok
}
