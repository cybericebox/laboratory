// Command headprog is a helper of the demux e2e (hack/kind-demux/e2e.sh): it puts a program that only hands packets
// on (TCX_NEXT) at the head of a device's tcx ingress and egress, in front of the demux's programs, and takes it away
// again. The e2e runs it on a kind node to check that the demux puts itself first again within its resync interval.
//
//	headprog attach <device>   # pinned under /sys/fs/bpf/demux-e2e-head
//	headprog remove <device>
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "attach" && os.Args[1] != "remove") {
		fmt.Fprintln(os.Stderr, "usage: headprog attach|remove <device>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "headprog:", err)
		os.Exit(1)
	}
}
