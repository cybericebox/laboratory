// Package profiles is the fixed catalog of device security profiles. A profile has a stable ID; the laboratory
// settings only say which catalog profiles are enabled on the cluster, and authors choose among them: there is no
// per-capability choice. The catalog lives in the code on purpose: capabilities are a platform decision.
package profiles

import "sort"

// The IDs of the catalog (also the names the frontends translate).
const (
	Standard = "standard"
	Extended = "extended"
)

// TUNResource is the extended resource a device plugin of the node-agent advertises; a pod that requests it gets
// /dev/net/tun and nothing else from the host.
const TUNResource = "cybericebox.com/tun"

// PingGroupRange is the value of net.ipv4.ping_group_range of every device: unprivileged ICMP echo sockets for every
// group, so `ping` works without NET_RAW.
const PingGroupRange = "0 2147483647"

// Base is the capability set of every device after "drop ALL". Privilege escalation stays allowed (sudo, setuid).
var Base = []string{
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "NET_BIND_SERVICE", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// Never are the capabilities no profile (and no setting) adds.
var Never = []string{
	"SYS_ADMIN", "SYS_MODULE", "SYS_RAWIO", "SYS_TIME", "SYS_BOOT", "DAC_READ_SEARCH", "BPF", "PERFMON", "SYSLOG",
	"AUDIT_CONTROL", "MAC_ADMIN", "MAC_OVERRIDE", "MKNOD",
}

// Profile is one entry of the catalog.
type Profile struct {
	ID string
	// Caps are added to Base.
	Caps []string
	// TUN: the device gets /dev/net/tun through the device plugin.
	TUN bool
}

var standardCaps = []string{"SYS_PTRACE", "IPC_LOCK", "LINUX_IMMUTABLE"}

var catalog = map[string]Profile{
	Standard: {ID: Standard, Caps: standardCaps},
	Extended: {ID: Extended, Caps: append(append([]string{}, standardCaps...), "NET_RAW", "NET_ADMIN"), TUN: true},
}

// aliases are the old preset names the specs of existing exercises carry.
var aliases = map[string]string{
	"": Standard, "basic": Standard, "service": Standard, "net": Extended, "debug": Extended,
}

// Resolve maps a name from a spec (a catalog ID, an old alias or empty) to a catalog ID.
func Resolve(name string) (string, bool) {
	if _, ok := catalog[name]; ok {
		return name, true
	}
	id, ok := aliases[name]
	return id, ok
}

// Get is the profile of a name from a spec; an unknown name gets the standard profile (the CRD refuses it anyway).
func Get(name string) Profile {
	id, ok := Resolve(name)
	if !ok {
		id = Standard
	}
	return catalog[id]
}

// IDs lists the catalog.
func IDs() []string {
	out := make([]string, 0, len(catalog))
	for id := range catalog {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Enabled is the validated list of profile IDs the cluster offers; unknown IDs are an error.
func Enabled(list []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, id := range list {
		if id == "" {
			continue
		}
		if _, ok := catalog[id]; !ok {
			return nil, &UnknownError{id}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// UnknownError names a profile that is not in the catalog.
type UnknownError struct{ ID string }

func (e *UnknownError) Error() string { return "unknown device profile " + e.ID }

// IsEnabled says whether the profile a spec names (an ID or an alias) is among the enabled IDs.
func IsEnabled(name string, enabled []string) bool {
	id, ok := Resolve(name)
	if !ok {
		return false
	}
	for _, e := range enabled {
		if e == id {
			return true
		}
	}
	return false
}
