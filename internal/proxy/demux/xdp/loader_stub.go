//go:build !linux

package xdp

import "net"

// XDPHandle is a no-op on non-linux platforms.
type XDPHandle struct{}

func Load(_ string, _ uint16) (*XDPHandle, error) {
	return nil, nil
}
func (h *XDPHandle) Update(_ uint32, _ net.IP, _ uint16, _ net.IP, _ uint16) error { return nil }
func (h *XDPHandle) Delete(_ uint32) error                     { return nil }
func (h *XDPHandle) Close() error                              { return nil }
