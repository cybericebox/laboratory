//go:build linux

package gateway

import (
	"bytes"
	"net"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cybericebox/laboratory/internal/nstest"
)

// The gateway announces its address to the lab at open: a gratuitous request and a gratuitous reply with the leg's own hardware address.
func TestNetnsAnnounceARPReachesTheSegment(t *testing.T) {
	nstest.Require(t)
	nstest.Run(t, "", "ip", "link", "add", "gwleg", "type", "veth", "peer", "name", "devleg")
	t.Cleanup(func() { _, _ = nstest.Try("", "ip", "link", "del", "gwleg") })
	nstest.Run(t, "", "ip", "link", "set", "gwleg", "address", "0a:00:00:00:00:01")
	nstest.Run(t, "", "ip", "link", "set", "gwleg", "up")
	nstest.Run(t, "", "ip", "link", "set", "devleg", "up")
	dev, err := net.InterfaceByName("devleg")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(arpProto))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: arpProto, Ifindex: dev.Index}); err != nil {
		t.Fatal(err)
	}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3})

	if err := announceARP("gwleg", net.ParseIP("10.192.3.1")); err != nil {
		t.Fatal(err)
	}
	ops := map[uint16]bool{}
	buf := make([]byte, 128)
	for len(ops) < 2 {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			break
		}
		if n < 42 {
			continue
		}
		mac := net.HardwareAddr{0x0a, 0, 0, 0, 0, 1}
		if !bytes.Equal(buf[6:12], mac) || !bytes.Equal(buf[22:28], mac) || !bytes.Equal(buf[28:32], net.ParseIP("10.192.3.1").To4()) || !bytes.Equal(buf[38:42], net.ParseIP("10.192.3.1").To4()) {
			t.Fatalf("not the announcement of the gateway: % x", buf[:n])
		}
		ops[uint16(buf[20])<<8|uint16(buf[21])] = true
	}
	if !ops[1] || !ops[2] {
		t.Fatalf("a gratuitous request and a reply are expected, got %v", ops)
	}
}
