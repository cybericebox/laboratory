package names

// WGPrivateKeyPlaceholder is the sentinel the controller writes in place of the
// WireGuard private key inside the client config it assembles into the
// LabGroupClient status. The cluster never holds the private key; the agent (or
// a direct caller) substitutes the real private key it generated for this
// sentinel before handing the config to the user. It cannot occur in a real
// config (WireGuard keys are base64, no underscores).
const WGPrivateKeyPlaceholder = "__PRIVATE_KEY__"

// WireGuardPort is the UDP port of every WireGuard endpoint of the platform: the VPN pod of a group listens on it, the wg-demux of
// the proxy binds it and forwards the handshakes of the groups to it, and the LoadBalancer Service publishes it. The public
// address a client is given (PUBLIC_VPN_ENDPOINT) may name another port when something in front maps it.
const WireGuardPort = 51820
