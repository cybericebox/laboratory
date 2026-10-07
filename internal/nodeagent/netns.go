//go:build linux

package nodeagent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
)

// conntrackSysctls are the per-netns switches of conntrack byte accounting and flow start times.
var conntrackSysctls = []string{"nf_conntrack_acct", "nf_conntrack_timestamp"}

// ConntrackAccountingOn reports whether both switches are already on in the target netns. Reading works with the read-only
// /proc/sys of the node-agent container; the node prep (the chart's host-prep init container, or the node image) sets them
// on the host and through the nf_conntrack module parameters, so a new pod namespace normally starts with them on.
func ConntrackAccountingOn(netnsPath string) bool {
	for _, f := range conntrackSysctls {
		out, err := nsenterOutput(netnsPath, "cat", "/proc/sys/net/netfilter/"+f)
		if err != nil || strings.TrimSpace(out) != "1" {
			return false
		}
	}
	return true
}

// EnableConntrackAccounting makes sure conntrack byte accounting and flow timestamps are on in the target netns: nothing to
// do when they already are, otherwise it tries to write them. The write only works when /proc/sys is writable; in the
// unprivileged node-agent container it is not, and then the error says so (the node prep is the place to set them).
func EnableConntrackAccounting(netnsPath string) error {
	if ConntrackAccountingOn(netnsPath) {
		return nil
	}
	for _, f := range conntrackSysctls {
		if err := nsenterRun(netnsPath, "sh", "-c", "echo 1 > /proc/sys/net/netfilter/"+f); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}

// WaitForLink polls until the named link appears in the current netns or the deadline passes.
func WaitForLink(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := netlink.LinkByName(name); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("link %q did not appear within %s", name, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// peerInRoot waits only for a newly created peer. An existing peer normally
// lives in the pod's namespace, so polling for it in the root namespace delays
// every healthy attachment before its namespace recovery checks.
func peerInRoot(name string, created bool) bool {
	var wait time.Duration
	if created {
		wait = 200 * time.Millisecond
	}
	return WaitForLink(name, wait) == nil
}

// SetMACInNetNS sets the hardware address of an interface inside the target netns.
func SetMACInNetNS(netnsPath, ifaceName, mac string) error {
	return nsenterRun(netnsPath, "ip", "link", "set", ifaceName, "address", mac)
}

// MoveToNetNS moves an interface from the host netns to the target netns by path.
// Uses netlink directly to avoid iproute2 versions that reject absolute paths in
// "ip link set netns <path>".
func MoveToNetNS(ifaceName, netnsPath string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("find link %s: %w", ifaceName, err)
	}
	f, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer func() { _ = f.Close() }()
	if err := netlink.LinkSetNsFd(link, int(f.Fd())); err != nil {
		return fmt.Errorf("move %s to netns %s: %w", ifaceName, netnsPath, err)
	}
	return nil
}

// RenameInNetNS renames an interface inside a target netns.
// Does not change UP/DOWN state — interface stays UP if it was UP before the move.
func RenameInNetNS(netnsPath, oldName, newName string) error {
	return nsenterRun(netnsPath, "ip", "link", "set", oldName, "name", newName)
}

// CheckInNetNS returns nil if the named interface exists inside the target netns.
func CheckInNetNS(netnsPath, ifaceName string) error {
	return nsenterRun(netnsPath, "ip", "link", "show", ifaceName)
}

// DeleteInNetNS deletes an interface inside the target netns.
func DeleteInNetNS(netnsPath, ifaceName string) error {
	return nsenterRun(netnsPath, "ip", "link", "del", ifaceName)
}

// BringUpInNetNS brings an interface UP inside the target netns.
func BringUpInNetNS(netnsPath, ifaceName string) error {
	return nsenterRun(netnsPath, "ip", "link", "set", ifaceName, "up")
}

func nsenterOutput(netnsPath string, args ...string) (string, error) {
	full := append([]string{"--net=" + netnsPath, "--"}, args...)
	out, err := exec.Command("nsenter", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func nsenterRun(netnsPath string, args ...string) error {
	full := append([]string{"--net=" + netnsPath, "--"}, args...)
	out, err := exec.Command("nsenter", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
