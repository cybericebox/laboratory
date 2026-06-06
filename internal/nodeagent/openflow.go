//go:build linux

package nodeagent

import (
	"encoding/binary"
	"fmt"
	"net"
	"path/filepath"

	"github.com/cybericebox/laboratory/internal/nodeagent/ofclient"
)

// FlowManager programs OpenFlow rules on br-ovs via the Go OF 1.3 client.
// Table 0: ingress — classify by in_port or tun_id.
type FlowManager struct {
	client *ofclient.Client
}

func NewFlowManager(ovsRunDir, bridge string) (*FlowManager, error) {
	sockPath := filepath.Join(ovsRunDir, bridge+".mgmt")
	c, err := ofclient.Connect(sockPath)
	if err != nil {
		return nil, fmt.Errorf("OF client connect to %s: %w", sockPath, err)
	}
	return &FlowManager{client: c}, nil
}

// Close closes the underlying OpenFlow connection.
func (f *FlowManager) Close() error {
	return f.client.Close()
}

// portNo resolves a named port, refreshing portMap once on miss.
func (f *FlowManager) portNo(name string) (uint32, error) {
	no, err := f.client.PortNo(name)
	if err != nil {
		if rerr := f.client.RefreshPorts(); rerr != nil {
			return 0, rerr
		}
		return f.client.PortNo(name)
	}
	return no, nil
}

// AddEgressFlow: local port → set tun_id=VNI + tun_dst=remoteVTEP → output via
// the single shared Geneve port. Uses NXM_NX_TUN_IPV4_DST (Nicira extension)
// so one Geneve port serves every remote VTEP — spec §7 "Один Geneve-порт на
// ноде".
func (f *FlowManager) AddEgressFlow(localPort, genevePort string, vni uint, remoteVTEP string) error {
	localNo, err := f.portNo(localPort)
	if err != nil {
		return err
	}
	geneveNo, err := f.portNo(genevePort)
	if err != nil {
		return err
	}
	vtepBE, err := ipv4BE(remoteVTEP)
	if err != nil {
		return fmt.Errorf("parse remote VTEP %q: %w", remoteVTEP, err)
	}
	// Match on in_port=local + dst-VTEP would over-narrow without MAC learning;
	// match in_port=local only and let the action set tun_dst.
	match := ofclient.BuildMatch(localNo, 0, false)
	var actions []byte
	actions = append(actions, ofclient.BuildActionsSetFieldTunnelID(uint64(vni))...)
	actions = append(actions, ofclient.BuildActionsSetTunDst(vtepBE)...)
	actions = append(actions, ofclient.BuildActionsOutput(geneveNo)...)
	return f.client.FlowAdd(0, 100, match, actions)
}

// ipv4BE parses a dotted-quad IPv4 address into a big-endian uint32 suitable
// for NXM_NX_TUN_IPV4_DST / NXM_NX_TUN_IPV4_SRC values.
func ipv4BE(s string) (uint32, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, fmt.Errorf("invalid IP")
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return 0, fmt.Errorf("IPv4 required")
	}
	return binary.BigEndian.Uint32(ip4), nil
}

// AddIngressFlow: geneve port + tun_id=VNI → output to local port.
func (f *FlowManager) AddIngressFlow(genevePort, localPort string, vni uint) error {
	geneveNo, err := f.portNo(genevePort)
	if err != nil {
		return err
	}
	localNo, err := f.portNo(localPort)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(geneveNo, uint64(vni), true)
	actions := ofclient.BuildActionsOutput(localNo)
	return f.client.FlowAdd(0, 100, match, actions)
}

// AddLocalSwitchFlow: port in VNI segment → set tunnel_id + normal L2 forwarding (same-node pods).
func (f *FlowManager) AddLocalSwitchFlow(localPort string, vni uint) error {
	localNo, err := f.portNo(localPort)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(localNo, 0, false)
	var actions []byte
	actions = append(actions, ofclient.BuildActionsSetFieldTunnelID(uint64(vni))...)
	actions = append(actions, ofclient.BuildActionsGroupNormal()...)
	return f.client.FlowAdd(0, 90, match, actions)
}

// DelFlowsByPort removes all table=0 flows matching in_port=portName.
func (f *FlowManager) DelFlowsByPort(portName string) error {
	portNo, err := f.portNo(portName)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(portNo, 0, false)
	return f.client.FlowDelete(0, match)
}

// DelFlowsByVNI removes table=0 flows matching in_port=genevePort and tunnel_id=VNI.
func (f *FlowManager) DelFlowsByVNI(vni uint, genevePort string) error {
	geneveNo, err := f.portNo(genevePort)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(geneveNo, uint64(vni), true)
	return f.client.FlowDelete(0, match)
}
