//go:build linux

package vpn

import (
	"fmt"
	"syscall"
)

// bindToDevice makes a listener take connections that arrive on one interface only: whatever address a packet is sent to, one
// that comes in on another interface (a lab interface) never reaches the socket.
func bindToDevice(iface string) func(network, address string, c syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
		}); err != nil {
			return err
		}
		if sockErr != nil {
			return fmt.Errorf("bind the socket to %s: %w", iface, sockErr)
		}
		return nil
	}
}
