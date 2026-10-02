// Package egress holds what a lab can never reach through its internet gateway. It is a constant of the code, not a
// setting: a lab needs no internal service and must not learn where it runs. Only the public internet is left.
package egress

// DenyV4 are the IPv4 destinations a lab never reaches: the private ranges (so the node, VPC, pod and service networks
// of a typical cluster and an internal load balancer), link-local (the cloud metadata service), loopback, CGNAT, the
// "this network", protocol-assignment and benchmarking ranges, and multicast and reserved. 168.63.129.16 is Azure's WireServer
// (a public address that answers only inside Azure, with the VM's configuration and secrets), denied wherever the cluster runs.
var DenyV4 = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8", "0.0.0.0/8",
	"192.0.0.0/24", "198.18.0.0/15", "224.0.0.0/3", "168.63.129.16/32",
}

// DenyV6 are the IPv6 equivalents: unspecified and loopback, IPv4-mapped and NAT64 (which reach IPv4 destinations),
// unique local, link-local, multicast, the discard and documentation prefixes.
var DenyV6 = []string{
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
}
