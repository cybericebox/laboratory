//go:build !linux

package gateway

import "net"

// announceARP is a gateway pod's business; it runs on Linux only.
func announceARP(string, net.IP) error { return nil }
