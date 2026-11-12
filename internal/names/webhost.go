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

var base36Radix = big.NewInt(36)

// NewWebCode returns n random lowercase base36 characters from crypto/rand.
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
	for _, m := range kindMarkers {
		if strings.HasSuffix(name, "--"+m) {
			return fmt.Errorf("device name %q may not end in %q, the mark of an access host", name, "--"+m)
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
// host name alone, never by a path or a query.
const (
	AccessKindHTTP     = "http"
	AccessKindTerminal = "terminal"
	AccessKindVNC      = "vnc"
	AccessKindRDP      = "rdp"
)

// kindMarkers are the fixed suffixes of the host labels of the kinds other than http. The web host <device>-<code> ends in a hyphen
// and a code of 3 to 4 letters or digits, so it never has "--" in its last 6 characters (a device name does not end in a hyphen
// either); a kind host ends in "--<marker>". Neither can be the other, whatever the names of the devices are.
var kindMarkers = map[string]string{AccessKindTerminal: "term", AccessKindVNC: "vnc", AccessKindRDP: "rdp"}

// AccessHostLabel is the first DNS label of the host that serves a kind of access to a device: the web host for http, and
// <device>-<code>--<marker> for the others. ok is false for an unknown kind. The code is the device's own (Device.Spec.Code); a device
// without one has no access host except the one its web Service already has.
func AccessHostLabel(device, code, kind string) (label string, ok bool) {
	if kind == AccessKindHTTP {
		return WebHostLabel(device, code), true
	}
	m, found := kindMarkers[kind]
	if !found {
		return "", false
	}
	return WebHostLabel(device, code) + "--" + m, true
}

// ParseAccessHost splits the first label of a host into the web host label of the device (<device>-<code>) and the kind. A label
// without a kind marker is a plain web host: kind is http and base is the label itself. A label that carries a marker of a kind is
// that kind. Nothing else decides which kind a host is, so a site that frames a console host (kind terminal) decides by this alone.
func ParseAccessHost(label string) (base, kind string) {
	for k, m := range kindMarkers {
		if b, ok := strings.CutSuffix(label, "--"+m); ok {
			return b, k
		}
	}
	return label, AccessKindHTTP
}
