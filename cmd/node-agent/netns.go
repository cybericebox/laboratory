//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// FindPodNetNS scans /proc to find the netns path for a pod by matching its UID in cgroup entries.
func FindPodNetNS(podUID string) (string, error) {
	return findPodNetNSIn("/proc", podUID)
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
		if strings.Contains(string(cgroup), podUID) {
			return filepath.Join(procRoot, e.Name(), "ns", "net"), nil
		}
	}
	return "", fmt.Errorf("pod %s netns not found in %s", podUID, procRoot)
}

// MoveToNetNS moves an interface from the host netns to the target netns by path.
func MoveToNetNS(ifaceName, netnsPath string) error {
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer ns.Close()

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("link %q not found in host netns: %w", ifaceName, err)
	}
	return netlink.LinkSetNsFd(link, int(ns))
}

// RenameInNetNS renames an interface inside a target netns.
func RenameInNetNS(netnsPath, oldName, newName string) error {
	return inNetNS(netnsPath, func() error {
		link, err := netlink.LinkByName(oldName)
		if err != nil {
			return fmt.Errorf("link %q: %w", oldName, err)
		}
		return netlink.LinkSetName(link, newName)
	})
}

// ConfigureInNetNS sets MAC, IP/prefix, and brings the interface up inside target netns.
// mac may be empty (skip). cidr is e.g. "192.168.1.1/24".
func ConfigureInNetNS(netnsPath, ifaceName, mac, cidr string) error {
	return inNetNS(netnsPath, func() error {
		link, err := netlink.LinkByName(ifaceName)
		if err != nil {
			return fmt.Errorf("link %q: %w", ifaceName, err)
		}
		if mac != "" {
			hw, err := net.ParseMAC(mac)
			if err != nil {
				return fmt.Errorf("parse MAC %q: %w", mac, err)
			}
			if err := netlink.LinkSetHardwareAddr(link, hw); err != nil {
				return err
			}
		}
		if cidr != "" {
			addr, err := netlink.ParseAddr(cidr)
			if err != nil {
				return fmt.Errorf("parse addr %q: %w", cidr, err)
			}
			if err := netlink.AddrAdd(link, addr); err != nil {
				return err
			}
		}
		return netlink.LinkSetUp(link)
	})
}

// inNetNS executes fn inside the netns at netnsPath, restoring the caller's netns on return.
// The OS thread is locked for the duration to prevent goroutine migration.
func inNetNS(netnsPath string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer origNS.Close()
	defer netns.Set(origNS) //nolint:errcheck

	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("set netns: %w", err)
	}
	return fn()
}
