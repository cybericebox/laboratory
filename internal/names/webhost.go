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
	// device + "-" + a code of at most WebCodeMaxLen chars + one kind character. 35 is a round,
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

var base36Radix = big.NewInt(36)

// NewWebCode returns n random lowercase base36 characters from crypto/rand. This is the code a device stores (Device.spec.code); the
// kind of access is not part of it (see AccessHostLabel).
func NewWebCode(n int) (string, error) {
	out := make([]byte, n)
	for i := range out {
		d, err := rand.Int(rand.Reader, base36Radix)
		if err != nil {
			return "", fmt.Errorf("names: random web code: %w", err)
		}
		out[i] = base36[d.Int64()]
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

// WebHostLabel is the first DNS label of a web-exposed device, both as the name of its Service and as the host under the base domain:
// <device>-<code>w, the access host of the kind http (AccessHostLabel). The code is random and kept short; uniqueness inside the
// group namespace is settled by Kubernetes itself, because the operator creates the Service with this name and picks another code
// on AlreadyExists. The device name must already satisfy ValidateDeviceName; it is not truncated here.
func WebHostLabel(device, code string) string {
	return device + "-" + code + string(kindChars[AccessKindHTTP])
}

// WorkloadName is the name of the Deployment of a device, and the prefix of its
// bare pods: <device>-<code>, without the kind character an access host has. The lab is
// never part of it; the relation goes through owner references and labels. A
// device without a code (created before codes existed) keeps its legacy name.
func WorkloadName(device, code, legacy string) string {
	if code == "" {
		return legacy
	}
	return device + "-" + code
}

// Access kinds of a device, as the host of the L7 proxy addresses them. Every access to a device through the proxy is addressed by a
// host name alone, never by a path or a query, and every such host looks the same: one label <device>-<code><kind> under the labs
// domain, where <code> is the random code of the device and <kind> is ONE character that only says which access it is.
const (
	AccessKindHTTP     = "http"
	AccessKindTerminal = "terminal"
	AccessKindVNC      = "vnc"
	AccessKindRDP      = "rdp"
	// AccessKindUnknown is what ParseAccessHost says of a kind character that stands for nothing (yet).
	AccessKindUnknown = "unknown"
)

// kindChars are the kind characters: w web, t terminal (the console), v vnc, r rdp. Every other character is reserved.
var kindChars = map[string]byte{AccessKindHTTP: 'w', AccessKindTerminal: 't', AccessKindVNC: 'v', AccessKindRDP: 'r'}

func validCode(code string) bool {
	if code == "" {
		return false
	}
	for i := 0; i < len(code); i++ {
		if c := code[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// AccessHostLabel is the first DNS label of the host that serves a kind of access to a device: <device>-<code><kind character>
// (w web, t terminal, v vnc, r rdp). ok is false for an unknown kind or a code that is not lowercase letters and digits.
func AccessHostLabel(device, code, kind string) (label string, ok bool) {
	c, found := kindChars[kind]
	if !found || !validCode(code) {
		return "", false
	}
	return device + "-" + code + string(c), true
}

// ParseAccessHost reads the first label of a host as an access host: the device name, the code of the device and the kind. The label
// is split at its last hyphen (a code has none), the last character is the kind and the rest is the code, whatever its length. A
// label of another shape (no hyphen, a bad device name, a code that is not lowercase letters and digits) returns an empty kind: it is
// no access host and the caller looks it up as it always did. A kind character that stands for nothing returns AccessKindUnknown. The
// rule for a client that has no code: a console host is one label under the labs domain whose last character is "t".
func ParseAccessHost(label string) (device, code, kind string) {
	i := strings.LastIndexByte(label, '-')
	if i <= 0 || len(label)-i < 3 {
		return "", "", ""
	}
	device, rest := label[:i], label[i+1:]
	code, last := rest[:len(rest)-1], rest[len(rest)-1]
	if ValidateDeviceName(device) != nil || !validCode(code) || !validCode(string(last)) {
		return "", "", ""
	}
	for k, c := range kindChars {
		if c == last {
			return device, code, k
		}
	}
	return device, code, AccessKindUnknown
}
