//go:build !linux

package vpn

import "syscall"

// bindToDevice is a Linux feature; the VPN pod runs on Linux only (this keeps the package building elsewhere).
func bindToDevice(string) func(network, address string, c syscall.RawConn) error { return nil }
