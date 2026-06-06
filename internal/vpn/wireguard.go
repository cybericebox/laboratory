//go:build linux

package vpn

import (
	"fmt"
	"net"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// persistentKeepalive matches spec §4 recommendation of 10-25 s for roaming
// recovery. 15 s is the midpoint and matches the client-side wg.conf rendered
// by the operator (see internal/controller/laboratory/labgroupclient_controller.go).
const persistentKeepalive = 15 * time.Second

type WGManager struct {
	client *wgctrl.Client
	iface  string
}

func NewWGManager(iface string) (*WGManager, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wgctrl.New: %w", err)
	}
	return &WGManager{client: c, iface: iface}, nil
}

func (m *WGManager) Init(privKeyBase64 string, listenPort int) error {
	key, err := wgtypes.ParseKey(privKeyBase64)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	return m.client.ConfigureDevice(m.iface, wgtypes.Config{
		PrivateKey: &key,
		ListenPort: &listenPort,
	})
}

func (m *WGManager) AddPeer(pubKeyBase64 string, allowedIP string) error {
	key, err := wgtypes.ParseKey(pubKeyBase64)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}
	_, ipNet, err := net.ParseCIDR(allowedIP)
	if err != nil {
		return fmt.Errorf("parse allowedIP %q: %w", allowedIP, err)
	}
	keepalive := persistentKeepalive
	return m.client.ConfigureDevice(m.iface, wgtypes.Config{
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   key,
			ReplaceAllowedIPs:           true,
			AllowedIPs:                  []net.IPNet{*ipNet},
			PersistentKeepaliveInterval: &keepalive,
		}},
	})
}

func (m *WGManager) RemovePeer(pubKeyBase64 string) error {
	key, err := wgtypes.ParseKey(pubKeyBase64)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}
	return m.client.ConfigureDevice(m.iface, wgtypes.Config{
		Peers: []wgtypes.PeerConfig{{PublicKey: key, Remove: true}},
	})
}

func (m *WGManager) Device() (*wgtypes.Device, error) {
	return m.client.Device(m.iface)
}

func (m *WGManager) Close() { m.client.Close() }
