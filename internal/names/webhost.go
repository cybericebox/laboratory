package names

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

const (
	// MaxDeviceNameLen is the longest device name a lab may have. A web-exposed
	// device is served at <device>-<code>.<base domain>, and the wildcard cert
	// covers one label level, so <device>-<code> must fit one DNS label (<=63):
	// device + "-" + a code of at most WebCodeMaxLen chars. 35 is a round,
	// predictable limit with plenty of headroom. Longer names are rejected, never
	// truncated: truncation could make two devices collide.
	MaxDeviceNameLen = 35

	// WebCodeLen is the usual length of the random code of a web host label.
	WebCodeLen = 3
	// WebCodeMaxLen is the longest code ever used; it is the fallback length
	// after WebCodeAttempts collisions at WebCodeLen.
	WebCodeMaxLen = 4
	// WebCodeAttempts is how many codes of WebCodeLen are tried before the
	// operator widens the code to WebCodeMaxLen.
	WebCodeAttempts = 8

	base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
)

var (
	base36Radix    = big.NewInt(36)
	kindSlotsRadix = big.NewInt(36 / KindSlots)
)

// NewWebCode returns a random base code of n lowercase base36 characters from crypto/rand. A base code is the code a device stores
// (Device.spec.code): its last character is one of the six that stand for the access kind http (the base36 index is a multiple of
// KindSlots), so the other kinds of access to the device can be written in the same position (see AccessHostLabel). The first n-1
// characters are free, which leaves 36^(n-1)*KindSlots codes of length n (a sixth of the space of fully random codes); the widening
// to WebCodeMaxLen after WebCodeAttempts collisions is unchanged.
func NewWebCode(n int) (string, error) {
	out := make([]byte, n)
	for i := range out {
		radix := base36Radix
		if i == n-1 {
			radix = kindSlotsRadix
		}
		d, err := rand.Int(rand.Reader, radix)
		if err != nil {
			return "", fmt.Errorf("names: random web code: %w", err)
		}
		v := d.Int64()
		if i == n-1 {
			v *= KindSlots
		}
		out[i] = base36[v]
	}
	return string(out), nil
}

// ValidateDeviceName checks a device name against the DNS label rules that the
// web host label <device>-<code> imposes: at most MaxDeviceNameLen characters
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
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("device name %q may contain only lowercase a-z, 0-9 and '-'", name)
		}
	}
	return nil
}

// ValidateNewDeviceName is ValidateDeviceName plus the reserved names. It applies to what a caller submits; a device that
// already exists under a reserved name keeps working because system pods are selected by LabelComponent, not by name.
func ValidateNewDeviceName(name string) error {
	if err := ValidateDeviceName(name); err != nil {
		return err
	}
	for _, r := range ReservedDeviceNames {
		if name == r {
			return fmt.Errorf("device name %q is reserved by the platform", name)
		}
	}
	return nil
}

// WebHostLabel is the first DNS label of a web-exposed device, both as the name
// of its Service and as the host under the base domain: <device>-<code>. The
// code is random and kept short; uniqueness inside the group namespace is
// settled by Kubernetes itself, because the operator creates the Service with
// this name and picks another code on AlreadyExists. The device name must
// already satisfy ValidateDeviceName; it is not truncated here.
func WebHostLabel(device, code string) string {
	return device + "-" + code
}

// WorkloadName is the name of the Deployment of a device, and the prefix of its
// bare pods: <device>-<code>, the same label the web Service has. The lab is
// never part of it; the relation goes through owner references and labels. A
// device without a code (created before codes existed) keeps its legacy name.
func WorkloadName(device, code, legacy string) string {
	if code == "" {
		return legacy
	}
	return WebHostLabel(device, code)
}

// Access kinds of a device, as the host of the L7 proxy addresses them. Every access to a device through the proxy is addressed by a
// host name alone, never by a path or a query, and every such host looks the same: one label <device>-<code> under the labs domain.
// The kind of access is the LAST character of the code: its base36 index is the index of the device's stored base code (a multiple of
// KindSlots) plus the index of the kind. There are KindSlots=6 slots; 4 and 5 are reserved.
const (
	AccessKindHTTP     = "http"
	AccessKindTerminal = "terminal"
	AccessKindVNC      = "vnc"
	AccessKindRDP      = "rdp"
	// AccessKindUnknown is what ParseAccessHost says of a reserved slot.
	AccessKindUnknown = "unknown"

	// KindSlots is the number of access kinds a host can tell apart.
	KindSlots = 6
)

var (
	kindIndex = map[string]int{AccessKindHTTP: 0, AccessKindTerminal: 1, AccessKindVNC: 2, AccessKindRDP: 3}
	kindNames = [KindSlots]string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP, AccessKindUnknown, AccessKindUnknown}
)

func base36Index(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	}
	return -1
}

// IsBaseCode says whether a code is a base code: the last character is one of the six that stand for http (index 0, 6, 12, 18, 24 or
// 30), and the code is made of lowercase letters and digits only.
func IsBaseCode(code string) bool {
	if code == "" {
		return false
	}
	for i := 0; i < len(code); i++ {
		if base36Index(code[i]) < 0 {
			return false
		}
	}
	return base36Index(code[len(code)-1])%KindSlots == 0
}

// AccessHostLabel is the first DNS label of the host that serves a kind of access to a device: <device>-<code> with the last character
// of the base code moved on by the index of the kind (http 0, terminal 1, vnc 2, rdp 3). For http it is the web host of the device, as
// it always was. ok is false for an unknown kind or a code that is not a base code (a device that predates the rule has no access host
// of another kind).
func AccessHostLabel(device, code, kind string) (label string, ok bool) {
	k, found := kindIndex[kind]
	if !found || !IsBaseCode(code) {
		return "", false
	}
	last := base36Index(code[len(code)-1])
	return device + "-" + code[:len(code)-1] + string(base36[last+k]), true
}

// ParseAccessHost reads the first label of a host as an access host: the device name, the base code of the device and the kind. A
// label that is not <device>-<code> (a code of WebCodeLen to WebCodeMaxLen lowercase letters or digits after the last hyphen, a valid
// device name before it) returns an empty kind: it is no access host, and the caller looks it up as it always did. A label whose last
// character is in a reserved slot returns AccessKindUnknown. The rule for a client that has no code: a console host is one label whose
// last character has a base36 index that leaves 1 when divided by 6.
func ParseAccessHost(label string) (device, code, kind string) {
	i := strings.LastIndexByte(label, '-')
	if i <= 0 {
		return "", "", ""
	}
	device, code = label[:i], label[i+1:]
	if len(code) < WebCodeLen || len(code) > WebCodeMaxLen || ValidateDeviceName(device) != nil {
		return "", "", ""
	}
	for j := 0; j < len(code); j++ {
		if base36Index(code[j]) < 0 {
			return "", "", ""
		}
	}
	last := base36Index(code[len(code)-1])
	return device, code[:len(code)-1] + string(base36[last-last%KindSlots]), kindNames[last%KindSlots]
}
