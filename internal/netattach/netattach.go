// Package netattach holds the rules of the device network interfaces and the
// encoding of the network.cybericebox.com/networks pod annotation shared by the
// operator (writer), the management agent (validation) and the node-agent (reader).
package netattach

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/cybericebox/laboratory/internal/names"
)

// InterfaceNamePattern is the shape of a device interface name chosen by a tenant:
// a DNS-label-like word of at most 15 characters (the Linux interface name limit).
// Keep it in sync with the CRD validation of InterfaceSpec.Name.
const InterfaceNamePattern = `^[a-z][a-z0-9-]{0,14}$`

var interfaceName = regexp.MustCompile(InterfaceNamePattern)

// reservedInterfaces cannot be chosen: they belong to the platform or to the kernel.
// eth0 is not reserved: a device with lab interfaces has no real eth0 (the CNI leaves a
// stub there), so the platform's own topologies name their first interface eth0.
var reservedInterfaces = map[string]bool{
	names.AccessPortIface: true,
	"lo":                  true,
}

// ValidateInterfaceName checks a tenant-chosen interface name.
func ValidateInterfaceName(n string) error {
	if !interfaceName.MatchString(n) {
		return fmt.Errorf("interface name %q: want %s", n, InterfaceNamePattern)
	}
	if reservedInterfaces[n] {
		return fmt.Errorf("interface name %q is reserved", n)
	}
	return nil
}

// ValidateMAC checks an interface MAC: empty, "random", or a unicast hardware address
// that is not all zeros.
func ValidateMAC(m string) error {
	if m == "" || m == "random" {
		return nil
	}
	hw, err := net.ParseMAC(m)
	if err != nil || len(hw) != 6 {
		return fmt.Errorf("mac %q is not a 6-byte hardware address", m)
	}
	if hw[0]&1 == 1 {
		return fmt.Errorf("mac %q is a multicast address", m)
	}
	if hw.String() == "00:00:00:00:00:00" {
		return fmt.Errorf("mac %q is the zero address", m)
	}
	return nil
}

// Attachment is one entry of the networks annotation.
type Attachment struct {
	// Iface is the interface name inside the pod netns.
	Iface string `json:"iface"`
	// Name is the OVS port name (set when a lab interface is attached later); empty
	// for a device interface whose connection is wired by the node-agent.
	Name string `json:"name,omitempty"`
	// MAC is the optional hardware address; empty keeps the generated one.
	MAC string `json:"mac,omitempty"`
}

// Encode renders the annotation value: a JSON array, so no character of a name or MAC
// can start another entry.
func Encode(list []Attachment) string {
	if len(list) == 0 {
		return ""
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// Parse reads the annotation value. A JSON array is the current format. The legacy
// "iface@[name][|MAC],..." form is still read so pods created before the upgrade keep
// working. Entries named "default" (handled by the CNI plugin) are excluded.
func Parse(annotation string) []Attachment {
	annotation = strings.TrimSpace(annotation)
	var out []Attachment
	if strings.HasPrefix(annotation, "[") {
		var list []Attachment
		if err := json.Unmarshal([]byte(annotation), &list); err != nil {
			return nil
		}
		for _, a := range list {
			if a.Iface != "" && a.Name != "default" {
				out = append(out, normalizeMAC(a))
			}
		}
		return out
	}
	for _, entry := range strings.Split(annotation, ",") {
		entry = strings.TrimSpace(entry)
		iface, rest, ok := strings.Cut(entry, "@")
		if !ok || iface == "" {
			continue
		}
		name, mac, _ := strings.Cut(rest, "|")
		if name == "default" {
			continue
		}
		out = append(out, normalizeMAC(Attachment{Iface: iface, Name: name, MAC: mac}))
	}
	return out
}

// normalizeMAC reads the MAC "random" as no MAC: the CNI cannot set a hardware address called "random", and the interface keeps the one it was
// generated with (which is what "random" asked for).
func normalizeMAC(a Attachment) Attachment {
	if a.MAC == "random" {
		a.MAC = ""
	}
	return a
}

// With returns the list with the attachment added (when absent).
func With(list []Attachment, a Attachment) []Attachment {
	for _, e := range list {
		if e == a {
			return list
		}
	}
	return append(append([]Attachment(nil), list...), a)
}

// Without returns the list without the attachment.
func Without(list []Attachment, a Attachment) []Attachment {
	out := make([]Attachment, 0, len(list))
	for _, e := range list {
		if e != a {
			out = append(out, e)
		}
	}
	return out
}
