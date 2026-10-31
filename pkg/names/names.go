// Package names holds the names of the laboratory that callers outside it (the daemon) need.
package names

// ReservedDeviceNames are the names of the endpoints the platform provides in every lab (the VPN, the internet
// gateway): a device cannot take them, and the agent refuses a lab that does. Callers validate their topologies
// against this list instead of copying it.
var ReservedDeviceNames = []string{"vpn", "gateway", "internet"}
