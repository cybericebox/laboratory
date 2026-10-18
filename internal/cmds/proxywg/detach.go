//go:build linux

package proxywg

import (
	"flag"
	"fmt"
	"os"

	"github.com/cybericebox/laboratory/internal/proxy/demux/fastpath"
)

// defaultPinPath mirrors the default of DEMUX_BPF_PIN_PATH.
const defaultPinPath = "/sys/fs/bpf/cybericebox/wg-demux"

// Detach is `proxy-wg detach`: it removes the fast path's links and pins from the node, after a switch from tc or xdp back to
// userspace (run once per node, in a one-off pod with the demux's tc privileges and the host's bpffs; see DEPLOY.md).
func Detach() {
	fs := flag.NewFlagSet("proxy-wg detach", flag.ExitOnError)
	pin := defaultPinPath
	if v := os.Getenv("DEMUX_BPF_PIN_PATH"); v != "" {
		pin = v
	}
	fs.StringVar(&pin, "pin-path", pin, "where the fast path is pinned")
	_ = fs.Parse(os.Args[2:])
	if err := fastpath.Detach(pin); err != nil {
		fmt.Fprintln(os.Stderr, "detach:", err)
		os.Exit(1)
	}
	fmt.Println("detached the fast path pinned at", pin)
}
