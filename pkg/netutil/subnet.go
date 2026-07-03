package netutil

import (
	"encoding/binary"
	"fmt"
	"net"
)

// SubnetForIndex computes a child subnet by adding n * 2^(32-childBits) to the
// literal host address in baseCIDR (NOT the masked network address).
// This lets you write "10.8.0.0/10" to mean "start from 10.8.0.0 within the /10 space".
// Example: SubnetForIndex("10.8.0.0/10", 24, 5) → "10.8.5.0/24"
func SubnetForIndex(baseCIDR string, childBits int, n uint) (string, error) {
	ip, ipNet, err := net.ParseCIDR(baseCIDR)
	if err != nil {
		return "", fmt.Errorf("parse base CIDR %q: %w", baseCIDR, err)
	}
	if childBits < 0 || childBits > 32 {
		return "", fmt.Errorf("childBits %d out of range", childBits)
	}
	base := binary.BigEndian.Uint32(ip.To4())
	blockSize := uint64(1) << uint(32-childBits)
	child := uint64(base) + uint64(n)*blockSize
	if child > uint64(^uint32(0)) {
		return "", fmt.Errorf("subnet index %d overflows IPv4 space from base %s", n, baseCIDR)
	}
	result := make(net.IP, 4)
	binary.BigEndian.PutUint32(result, uint32(child))
	if !ipNet.Contains(result) {
		return "", fmt.Errorf("subnet index %d escapes base network %s", n, baseCIDR)
	}
	return fmt.Sprintf("%s/%d", result, childBits), nil
}
