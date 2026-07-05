//go:build linux

// ovs-diag checks each step of moving an OVS internal port into a pod netns,
// comparing the library approach (vishvananda/netlink+netns) with the CLI (ip / nsenter).
//
// Run (inside the node-agent container):
//
//	ovs-diag \
//	  --netns  /host/proc/1234/ns/net \
//	  --bridge br-ovs \
//	  --sock   /var/run/openvswitch/db.sock
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func main() {
	netnsPath := flag.String("netns", "", "path to the pod's netns (e.g. /host/proc/1234/ns/net)")
	bridge := flag.String("bridge", "br-ovs", "OVS bridge name")
	sock := flag.String("sock", "/var/run/openvswitch/db.sock", "path to the OVSDB socket")
	flag.Parse()

	if *netnsPath == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --netns is required")
		os.Exit(1)
	}
	
	fmt.Printf("=== ovs-diag ===\nnetns: %s\nbridge: %s\nsock: %s\n\n", *netnsPath, *bridge, *sock)
	
	const portName = "icediag000001"
	
	// Remove any leftover port from a previous run.
	runOVSCtl(*sock, "del-port", *bridge, portName)

	step("1. ovs-vsctl add-port (create internal port)")
	if err := runOVSCtl(
		*sock, "add-port", *bridge, portName,
		"--", "set", "interface", portName, "type=internal",
	); err != nil {
		fatal("ovs-vsctl add-port: %v", err)
	}
	
	// Wait for the kernel interface to appear in root netns.
	step("1b. WaitForLink in root netns")
	if err := waitForLink(portName, 5*time.Second); err != nil {
		fatal("WaitForLink: %v", err)
	}
	printLink("root netns", portName)
	
	// Wait for a stable ifindex (vswitchd dpif_port_add).
	step("1c. waiting for a stable ifindex (300ms)")
	if err := waitStableIfindex(portName, 5*time.Second); err != nil {
		fatal("waitStableIfindex: %v", err)
	}

	step("2a. [LIB] MoveToNetNS: netlink.LinkSetNsFd")
	if err := libMoveToNetNS(portName, *netnsPath); err != nil {
		fmt.Printf("  FAIL: %v\n", err)
		fmt.Println("  → falling back to CLI")
		
		step("2b. [CLI] MoveToNetNS: ip link set netns")
		if err2 := cliMoveToNetNS(portName, *netnsPath); err2 != nil {
			fatal("CLI MoveToNetNS: %v", err2)
		}
		fmt.Println("  CLI OK")
	} else {
		fmt.Println("  LIB OK")
	}
	
	// Check that the interface disappeared from root netns and appeared in pod netns.
	if _, e := netlink.LinkByName(portName); e == nil {
		fmt.Printf("  WARNING: %s still in root netns!\n", portName)
	} else {
		fmt.Printf("  root netns: %s absent (expected)\n", portName)
	}
	fmt.Printf("  pod netns: ")
	if err := checkInNetNS(*netnsPath, portName); err != nil {
		fmt.Printf("FAIL (%v)\n", err)
	} else {
		fmt.Println("PRESENT")
	}
	
	const targetName = "diageth1"
	step(fmt.Sprintf("3a. [LIB] RenameInNetNS: %s → %s", portName, targetName))
	if err := libRenameInNetNS(*netnsPath, portName, targetName); err != nil {
		fmt.Printf("  FAIL: %v\n", err)
		fmt.Println("  → trying CLI")
		
		step(fmt.Sprintf("3b. [CLI] RenameInNetNS: nsenter ip link set name"))
		if err2 := cliRenameInNetNS(*netnsPath, portName, targetName); err2 != nil {
			fatal("CLI RenameInNetNS: %v", err2)
		}
		fmt.Println("  CLI OK")
	} else {
		fmt.Println("  LIB OK")
	}
	
	fmt.Printf("  pod netns %s: ", targetName)
	if err := checkInNetNS(*netnsPath, targetName); err != nil {
		fmt.Printf("FAIL (%v)\n", err)
	} else {
		fmt.Println("PRESENT")
	}
	
	step("4. Timing: how fast does vswitchd remove the interface after move?")
	t0 := time.Now()
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		err := checkInNetNS(*netnsPath, targetName)
		elapsed := time.Since(t0)
		if err != nil {
			fmt.Printf("  [%3dms] %s: GONE (%v)\n", elapsed.Milliseconds(), targetName, err)
			break
		}
		fmt.Printf("  [%3dms] %s: present\n", elapsed.Milliseconds(), targetName)
	}
	
	runOVSCtl(*sock, "del-port", *bridge, portName)
	time.Sleep(500 * time.Millisecond)

	step("5. TEST: move+rename WITHOUT LinkSetUp — how long does it survive?")
	if err := runOVSCtl(
		*sock, "add-port", *bridge, portName,
		"--", "set", "interface", portName, "type=internal",
	); err != nil {
		fatal("add-port: %v", err)
	}
	if err := waitForLink(portName, 5*time.Second); err != nil {
		fatal("WaitForLink: %v", err)
	}
	if err := waitStableIfindex(portName, 5*time.Second); err != nil {
		fatal("waitStableIfindex: %v", err)
	}
	if err := libMoveToNetNS(portName, *netnsPath); err != nil {
		fatal("MoveToNetNS: %v", err)
	}
	if err := libRenameInNetNS(*netnsPath, portName, targetName); err != nil {
		fatal("RenameInNetNS: %v", err)
	}
	fmt.Println("  moved+renamed, NOT calling LinkSetUp")
	t0 = time.Now()
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		err := checkInNetNS(*netnsPath, targetName)
		elapsed := time.Since(t0)
		if err != nil {
			fmt.Printf("  [%4dms] %s: GONE — vswitchd removed it\n", elapsed.Milliseconds(), targetName)
			break
		}
		fmt.Printf("  [%4dms] %s: present\n", elapsed.Milliseconds(), targetName)
	}

	fmt.Println("\n=== RESULT ===")
	fmt.Printf("See output above: when does vswitchd remove it, and does LinkSetUp trigger immediate removal\n")

	runOVSCtl(*sock, "del-port", *bridge, portName)
	cliRunInNetNS(*netnsPath, "ip", "link", "del", targetName)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func step(s string) { fmt.Printf("\n── %s\n", s) }

func fatal(format string, args ...any) {
	fmt.Printf("FATAL: "+format+"\n", args...)
	os.Exit(1)
}

func runOVSCtl(sock string, args ...string) error {
	full := append([]string{"--db=unix:" + sock}, args...)
	out, err := exec.Command("ovs-vsctl", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitForLink(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := netlink.LinkByName(name); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("link %q did not appear within %s", name, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitStableIfindex(portName string, timeout time.Duration) error {
	const stableDur = 300 * time.Millisecond
	deadline := time.Now().Add(timeout)
	var stableIdx int
	var stableSince time.Time
	for {
		link, err := netlink.LinkByName(portName)
		if err == nil {
			idx := link.Attrs().Index
			if idx != stableIdx {
				stableIdx = idx
				stableSince = time.Now()
			} else if time.Since(stableSince) >= stableDur {
				fmt.Printf("  ifindex=%d stable %.0fms\n", stableIdx, stableDur.Seconds()*1000)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ifindex did not stabilize within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func printLink(where, name string) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		fmt.Printf("  [%s] %s: NOT FOUND\n", where, name)
		return
	}
	fmt.Printf(
		"  [%s] %s: ifindex=%d type=%s flags=%v\n",
		where, name, link.Attrs().Index, link.Type(), link.Attrs().Flags,
	)
}

// ── library variants ──────────────────────────────────────────────────────────

func libMoveToNetNS(ifaceName, netnsPath string) error {
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("GetFromPath: %w", err)
	}
	defer ns.Close()
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("LinkByName: %w", err)
	}
	return netlink.LinkSetNsFd(link, int(ns))
}

func libRenameInNetNS(netnsPath, oldName, newName string) error {
	return inNetNS(
		netnsPath, func() error {
			link, err := netlink.LinkByName(oldName)
			if err != nil {
				return fmt.Errorf("LinkByName %q: %w", oldName, err)
			}
			return netlink.LinkSetName(link, newName)
		},
	)
}

func libConfigureInNetNS(netnsPath, ifaceName string) error {
	return inNetNS(
		netnsPath, func() error {
			link, err := netlink.LinkByName(ifaceName)
			if err != nil {
				return fmt.Errorf("LinkByName %q: %w", ifaceName, err)
			}
			return netlink.LinkSetUp(link)
		},
	)
}

func inNetNS(netnsPath string, fn func() error) error {
	runtime.LockOSThread()
	origNS, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("Get origNS: %w", err)
	}
	defer origNS.Close()
	
	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("GetFromPath %q: %w", netnsPath, err)
	}
	defer targetNS.Close()
	
	if err := netns.Set(targetNS); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("Set targetNS: %w", err)
	}
	
	fnErr := fn()
	
	if err := netns.Set(origNS); err != nil {
		panic(fmt.Sprintf("inNetNS: failed to restore origNS: %v", err))
	}
	runtime.UnlockOSThread()
	return fnErr
}

// ── CLI variants ──────────────────────────────────────────────────────────────

func cliMoveToNetNS(ifaceName, netnsPath string) error {
	out, err := exec.Command("ip", "link", "set", ifaceName, "netns", netnsPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link set netns: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cliRenameInNetNS(netnsPath, oldName, newName string) error {
	out, err := exec.Command(
		"nsenter", "--net="+netnsPath, "--",
		"ip", "link", "set", oldName, "name", newName,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nsenter rename: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cliConfigureInNetNS(netnsPath, ifaceName string) error {
	out, err := exec.Command(
		"nsenter", "--net="+netnsPath, "--",
		"ip", "link", "set", ifaceName, "up",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nsenter set up: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cliRunInNetNS(netnsPath string, args ...string) {
	exec.Command("nsenter", append([]string{"--net=" + netnsPath, "--"}, args...)...).Run() // nolint
}

func checkInNetNS(netnsPath, ifaceName string) error {
	return inNetNS(
		netnsPath, func() error {
			_, err := netlink.LinkByName(ifaceName)
			return err
		},
	)
}
