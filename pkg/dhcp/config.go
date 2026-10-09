package dhcp

// Config describes a DHCP server for one network interface.
// Subnet and Gateway are derived from the lab's allocated CIDR by the caller.
// DNS is optional — omit to suppress the DNS option in responses.
// BindIP is used as the DHCP Server Identifier (option 54) in replies; the
// socket itself always binds 0.0.0.0:67 — binding the unicast address would
// stop broadcast DISCOVERs (dst 255.255.255.255) from ever reaching the
// server. Per-lab isolation on a shared pod comes from SO_BINDTODEVICE
// (server4 sets it from Iface) plus SO_REUSEADDR.
type Config struct {
	Iface   string
	Subnet  string
	Gateway string
	DNS     string
	BindIP  string
	Ranges  []Range
}
