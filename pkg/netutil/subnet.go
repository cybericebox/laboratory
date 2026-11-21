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

// SubnetCount is the number of child subnets of childBits that SubnetForIndex can give from
// baseCIDR (numbers 0 .. count-1). It counts from the literal host address of the base, like
// SubnetForIndex: "10.8.0.0/10" has the subnets from 10.8.0.0 to the end of its /10.
func SubnetCount(baseCIDR string, childBits int) (uint, error) {
	ip, ipNet, err := net.ParseCIDR(baseCIDR)
	if err != nil || ip.To4() == nil {
		return 0, fmt.Errorf("parse base CIDR %q: %w", baseCIDR, err)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || childBits < ones || childBits > 32 {
		return 0, fmt.Errorf("child /%d does not fit base network %s", childBits, baseCIDR)
	}
	end := uint64(binary.BigEndian.Uint32(ipNet.IP.To4())) + uint64(1)<<uint(32-ones)
	start := uint64(binary.BigEndian.Uint32(ip.To4()))
	block := uint64(1) << uint(32-childBits)
	return uint((end - start) / block), nil
}

// SubnetIndex is the number of cidr among the child subnets of baseCIDR: the inverse of
// SubnetForIndex. The cidr must be a canonical subnet of childBits inside the base.
func SubnetIndex(baseCIDR string, childBits int, cidr string) (uint, error) {
	baseIP, baseNet, err := net.ParseCIDR(baseCIDR)
	if err != nil {
		return 0, fmt.Errorf("parse base CIDR %q: %w", baseCIDR, err)
	}
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	ones, _ := ipNet.Mask.Size()
	if ones != childBits || !ip.Equal(ipNet.IP) || !baseNet.Contains(ipNet.IP) || ip.To4() == nil {
		return 0, fmt.Errorf("%q is not a /%d subnet of %s", cidr, childBits, baseCIDR)
	}
	block := uint64(1) << uint(32-childBits)
	start := uint64(binary.BigEndian.Uint32(baseIP.To4()))
	at := uint64(binary.BigEndian.Uint32(ip.To4()))
	if at < start || (at-start)%block != 0 {
		return 0, fmt.Errorf("%q is not aligned to the subnets of %s", cidr, baseCIDR)
	}
	return uint((at - start) / block), nil
}
