package names

import "strconv"

const (
	// RegistryServiceAddr is host:port of the platform registry (zot) Service inside the cluster. The chart installs it in the release
	// namespace under this name and always installs it.
	RegistryServiceAddr = "laboratory-registry." + SystemNamespace + ".svc:5000"
	// RegistryForwardPort is the port on which the node-agent of every node relays the registry on the loopback, so that the container
	// runtime pulls from localhost:<port> (plain HTTP, no node configuration). It must be free on the node.
	RegistryForwardPort = 5035
)

// RegistryNodePrefix is host:port of the registry as the nodes see it (the node-agent's localhost forwarder): image references of the
// image cache and of the snapshots start with it.
var RegistryNodePrefix = "localhost:" + strconv.Itoa(RegistryForwardPort)
