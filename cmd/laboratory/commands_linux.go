//go:build linux

package main

import (
	"github.com/cybericebox/laboratory/internal/cmds/gateway"
	"github.com/cybericebox/laboratory/internal/cmds/installcni"
	"github.com/cybericebox/laboratory/internal/cmds/netconfig"
	"github.com/cybericebox/laboratory/internal/cmds/nodeagentd"
	"github.com/cybericebox/laboratory/internal/cmds/proxywg"
	"github.com/cybericebox/laboratory/internal/cmds/vpn"
)

func init() {
	commands["proxy-wg"] = proxywg.Run
	commands["vpn"] = vpn.Run
	commands["gateway"] = gateway.Run
	commands["node-agent"] = nodeagentd.Run
	commands["install-cni"] = installcni.Run
	commands["netconfig"] = netconfig.Run
}
