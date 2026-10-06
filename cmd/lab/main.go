//go:build linux

// lab is the per-lab-group image's binary: the VPN server and the internet gateway.
package main

import (
	"github.com/cybericebox/laboratory/internal/cmds/gateway"
	"github.com/cybericebox/laboratory/internal/cmds/vpn"
	"github.com/cybericebox/laboratory/internal/multicall"
)

func main() {
	multicall.Run(map[string]func(){
		"vpn":     vpn.Run,
		"gateway": gateway.Run,
	})
}
