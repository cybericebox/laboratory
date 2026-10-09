//go:build linux

package nstest

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"golang.org/x/sys/unix"
)

func DHCP(ns, server string, msg *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	args := helperArgv("dial", "dhcp", server+"|"+base64.StdEncoding.EncodeToString(msg.ToBytes()))
	if ns != "" {
		args = append([]string{"ip", netnsSubcommand, netnsExec, ns}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("DHCP client: %w: %s", err, out)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, err
	}
	return dhcpv4.FromBytes(raw)
}
func helperDHCP(value string) {
	server, encoded, _ := strings.Cut(value, "|")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		os.Exit(2)
	}
	msg, err := dhcpv4.FromBytes(raw)
	if err != nil {
		os.Exit(2)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 68})
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(3)
	}
	defer func() { _ = conn.Close() }()
	sys, _ := conn.SyscallConn()
	_ = sys.Control(func(fd uintptr) {
		ifaces, _ := net.Interfaces()
		for _, iface := range ifaces {
			if iface.Name != "lo" && iface.Flags&net.FlagUp != 0 {
				_ = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface.Name)
				break
			}
		}
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	})
	dst := net.ParseIP(server)
	if msg.IsBroadcast() {
		dst = net.IPv4bcast
	}
	if _, err := conn.WriteToUDP(raw, &net.UDPAddr{IP: dst, Port: 67}); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(3)
	}
	if msg.MessageType() == dhcpv4.MessageTypeRelease || msg.MessageType() == dhcpv4.MessageTypeDecline {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(4)
		}
		reply, err := dhcpv4.FromBytes(buf[:n])
		if err == nil && reply.TransactionID == msg.TransactionID {
			fmt.Print(base64.StdEncoding.EncodeToString(buf[:n]))
			os.Exit(0)
		}
	}
}
