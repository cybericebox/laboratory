//go:build linux

// node is the per-node image's binary: node-agent, install-cni, netconfig, cni-gate the lab checker
// (checker, checker-health) and the device access service (device-access, device-access-health).
// cni-gate is also started by its own name: the host copy in /opt/cni/bin/cni-gate.
package main

import (
	"github.com/cybericebox/laboratory/internal/cmds/checkerd"
	"github.com/cybericebox/laboratory/internal/cmds/cnigate"
	"github.com/cybericebox/laboratory/internal/cmds/deviceaccessd"
	"github.com/cybericebox/laboratory/internal/cmds/installcni"
	"github.com/cybericebox/laboratory/internal/cmds/netconfig"
	"github.com/cybericebox/laboratory/internal/cmds/nodeagentd"
	"github.com/cybericebox/laboratory/internal/multicall"
)

func main() {
	multicall.Run(map[string]func(){
		"node-agent":           nodeagentd.Run,
		"install-cni":          installcni.Run,
		"netconfig":            netconfig.Run,
		"cni-gate":             cnigate.Run,
		"checker":              checkerd.Run,
		"checker-health":       checkerd.Health,
		"device-access":        deviceaccessd.Run,
		"device-access-health": deviceaccessd.Health,
	})
}
