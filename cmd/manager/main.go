// manager is the operator image's binary.
package main

import (
	"os"

	"github.com/cybericebox/laboratory/internal/cmds/manager"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "egress-log" {
		os.Exit(manager.RunEgressLog(os.Args[2:]))
	}
	manager.Run()
}
