//go:build linux

package main

import (
	"fmt"
	"path/filepath"

	"github.com/cybericebox/laboratory/cmd/node-agent/ofclient"
)

// FlowManager programs OpenFlow rules on br-ovs via the Go OF 1.3 client.
// Table 0: ingress — classify by in_port or tun_id.
type FlowManager struct {
	client *ofclient.Client
}

func newFlowManager(ovsRunDir, bridge string) (*FlowManager, error) {
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

// AddEgressFlow: local port → set tunnel_id=VNI → output via geneve port.
func (f *FlowManager) AddEgressFlow(localPort, genevePort string, vni uint) error {
	localNo, err := f.client.PortNo(localPort)
	if err != nil {
		return err
	}
	geneveNo, err := f.client.PortNo(genevePort)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(localNo, 0, false)
	var actions []byte
	actions = append(actions, ofclient.BuildActionsSetFieldTunnelID(uint64(vni))...)
	actions = append(actions, ofclient.BuildActionsOutput(geneveNo)...)
	return f.client.FlowAdd(0, 100, match, actions)
}

// AddIngressFlow: geneve port + tun_id=VNI → output to local port.
func (f *FlowManager) AddIngressFlow(genevePort, localPort string, vni uint) error {
	geneveNo, err := f.client.PortNo(genevePort)
	if err != nil {
		return err
	}
	localNo, err := f.client.PortNo(localPort)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(geneveNo, uint64(vni), true)
	actions := ofclient.BuildActionsOutput(localNo)
	return f.client.FlowAdd(0, 100, match, actions)
}

// AddLocalSwitchFlow: port in VNI segment → set tunnel_id + normal L2 forwarding (same-node pods).
func (f *FlowManager) AddLocalSwitchFlow(localPort string, vni uint) error {
	localNo, err := f.client.PortNo(localPort)
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
	portNo, err := f.client.PortNo(portName)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(portNo, 0, false)
	return f.client.FlowDelete(0, match)
}

// DelFlowsByVNI removes table=0 flows matching in_port=genevePort and tunnel_id=VNI.
func (f *FlowManager) DelFlowsByVNI(vni uint, genevePort string) error {
	geneveNo, err := f.client.PortNo(genevePort)
	if err != nil {
		return err
	}
	match := ofclient.BuildMatch(geneveNo, uint64(vni), true)
	return f.client.FlowDelete(0, match)
}
