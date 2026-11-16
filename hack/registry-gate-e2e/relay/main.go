// Command relay is the registry relay of a node, as the node-agent runs it (devicestate.Forward with the same
// options), without the rest of the node-agent: the E2E of the registry gate runs it as a host-network pod, so
// containerd and the kubelet of a kind node pull from localhost:5035 through the real relay code, over TLS, to the
// gate.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/devicestate"
)

func main() {
	target, ca := os.Getenv("STATE_REGISTRY_ADDR"), os.Getenv("STATE_REGISTRY_CA_FILE")
	if target == "" {
		fmt.Fprintln(os.Stderr, "STATE_REGISTRY_ADDR is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	opts := []devicestate.ForwardOption{devicestate.WithHost("localhost:5035")}
	if ca != "" {
		opts = append(opts, devicestate.WithTLS(ca))
	}
	if err := devicestate.Forward(ctx, "127.0.0.1:5035", target, logr.Discard(), opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
