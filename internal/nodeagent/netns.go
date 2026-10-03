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

// EnableConntrackAccounting switches on conntrack byte accounting and flow timestamps in the target netns. They are
// off by default and /proc/sys is read-only for an unprivileged container, so the VPN pod (which carries the
// annotation names.AnnotationConntrackAccounting) cannot do it itself; the node-agent already works in pod
// namespaces. Without them the flow collector still counts attempts and replies, only bytes stay zero, so a failure
// is reported and never fails the pod.
func EnableConntrackAccounting(netnsPath string) error {
	var last error
	for _, f := range []string{"nf_conntrack_acct", "nf_conntrack_timestamp"} {
		ok := false
		for i := 0; i < 5 && !ok; i++ {
			if last = nsenterRun(netnsPath, "sh", "-c", "echo 1 > /proc/sys/net/netfilter/"+f); last == nil {
				ok = true
			} else {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if !ok {
			return fmt.Errorf("%s: %w", f, last)
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

func nsenterRun(netnsPath string, args ...string) error {
	full := append([]string{"--net=" + netnsPath, "--"}, args...)
	out, err := exec.Command("nsenter", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
