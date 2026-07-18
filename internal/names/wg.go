package names

// WGPrivateKeyPlaceholder is the sentinel the controller writes in place of the
// WireGuard private key inside the client config it assembles into the
// LabGroupClient status. The cluster never holds the private key; the agent (or
// a direct caller) substitutes the real private key it generated for this
// sentinel before handing the config to the user. It cannot occur in a real
// config (WireGuard keys are base64, no underscores).
const WGPrivateKeyPlaceholder = "__PRIVATE_KEY__"
