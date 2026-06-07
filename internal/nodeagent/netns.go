//go:build linux

package nodeagent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
)

// FindPodNetNS scans procRoot to find the netns path for a pod by matching its UID in cgroup entries.
func FindPodNetNS(procRoot, podUID string) (string, error) {
	return findPodNetNSIn(procRoot, podUID)
}

func findPodNetNSIn(procRoot, podUID string) (string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", procRoot, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cgroup, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cgroup"))
		if err != nil {
			continue
		}
		cgroupStr := string(cgroup)
		if strings.Contains(cgroupStr, podUID) || strings.Contains(cgroupStr, strings.ReplaceAll(podUID, "-", "_")) {
			return filepath.Join(procRoot, e.Name(), "ns", "net"), nil
		}
	}
	return "", fmt.Errorf("pod %s netns not found in %s", podUID, procRoot)
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
func MoveToNetNS(ifaceName, netnsPath string) error {
	out, err := exec.Command("ip", "link", "set", ifaceName, "netns", netnsPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link set %s netns %s: %w: %s", ifaceName, netnsPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RenameInNetNS renames an interface inside a target netns.
// Does not change UP/DOWN state — interface stays UP if it was UP before the move.
func RenameInNetNS(netnsPath, oldName, newName string) error {
	return nsenterRun(netnsPath, "ip", "link", "set", oldName, "name", newName)
}

// ConfigureInNetNS assigns an optional IP/prefix inside the target netns.
// cidr may be empty (skip). Example: "192.168.1.1/24".
func ConfigureInNetNS(netnsPath, ifaceName, cidr string) error {
	if cidr == "" {
		return nil
	}
	return nsenterRun(netnsPath, "ip", "addr", "add", cidr, "dev", ifaceName)
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
