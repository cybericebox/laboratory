package names

import (
	"net"
	"strconv"
	"strings"
)

// WGPrivateKeyPlaceholder is the sentinel the controller writes in place of the
// WireGuard private key inside the client config it assembles into the
// LabGroupClient status. The cluster never holds the private key; the agent (or
// a direct caller) substitutes the real private key it generated for this
// sentinel before handing the config to the user. It cannot occur in a real
// config (WireGuard keys are base64, no underscores).
const WGPrivateKeyPlaceholder = "__PRIVATE_KEY__"

// WireGuardPort is the UDP port of every WireGuard endpoint INSIDE the cluster network: the VPN pod of a group listens on it, the
// wg-demux of the proxy binds it and forwards the handshakes of the groups to it, and the LoadBalancer Service forwards to it. A fixed
// in-network port, not a setting. The port clients connect to, outside the cluster, is another matter (chart proxy.wg.publicPort, the
// port of PUBLIC_VPN_ENDPOINT): it defaults to this one.
const WireGuardPort = 51820

// WithPublicVPNPort completes the public WireGuard address advertised to clients (PUBLIC_VPN_ENDPOINT) with the default port when it
// names none: "vpn.example.com" becomes "vpn.example.com:51820". An address with a port is returned as it is. The chart already
// appends proxy.wg.publicPort; this is for a deploy without the chart.
func WithPublicVPNPort(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return endpoint
	}
	if _, _, err := net.SplitHostPort(endpoint); err == nil {
		return endpoint
	}
	return net.JoinHostPort(strings.Trim(endpoint, "[]"), strconv.Itoa(WireGuardPort))
}
