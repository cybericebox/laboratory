//go:build tools

// Package main holds tool dependencies that are not directly imported in
// production code but must be tracked in go.mod / go.sum.
package main

import (
	_ "github.com/cilium/ebpf"
	_ "github.com/coreos/go-iptables/iptables"
	_ "github.com/insomniacslk/dhcp/dhcpv4"
	_ "github.com/vishvananda/netlink"
)
