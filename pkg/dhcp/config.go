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
	// Routes are classless static routes (RFC 3442, option 121) sent to every client. A client that gets the option
	// ignores the router option, so the reply then carries the default route via Gateway as well.
	Routes []Route
}

// Route is a classless static route: Dst (an IPv4 CIDR) reachable via the IPv4 address Via.
type Route struct {
	Dst string
	Via string
}
