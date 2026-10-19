//go:build linux

package main

import (
	"os"

	"github.com/cybericebox/laboratory/internal/cmds/proxywg"
)

// `proxy-wg` runs the demux; `proxy-wg detach` removes its fast path from the node.
func init() {
	commands["proxy-wg"] = func() {
		if len(os.Args) > 1 && os.Args[1] == "detach" {
			proxywg.Detach()
			return
		}
		proxywg.Run()
	}
}
