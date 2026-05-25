//go:build linux

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// FlowManager programs OpenFlow rules on br-ovs via ovs-ofctl exec.
// Table 0: ingress — classify by in_port or tun_id.
type FlowManager struct {
	bridge string
}

func newFlowManager(bridge string) *FlowManager {
	return &FlowManager{bridge: bridge}
}

func (f *FlowManager) ofctl(args ...string) error {
	out, err := exec.Command("ovs-ofctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ovs-ofctl %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AddEgressFlow: local port → load VNI into tun_id → send via geneve port.
func (f *FlowManager) AddEgressFlow(localPort, genevePort string, vni uint) error {
	return f.ofctl("add-flow", f.bridge,
		fmt.Sprintf("table=0,priority=100,in_port=%s,actions=load:%d->NXM_NX_TUN_ID[],output:%s",
			localPort, vni, genevePort))
}

// AddIngressFlow: geneve port + tun_id=VNI → output local port.
func (f *FlowManager) AddIngressFlow(genevePort, localPort string, vni uint) error {
	return f.ofctl("add-flow", f.bridge,
		fmt.Sprintf("table=0,priority=100,in_port=%s,tun_id=%d,actions=output:%s",
			genevePort, vni, localPort))
}

// AddLocalSwitchFlow: port in VNI segment → flood within the VNI (L2 switch behaviour for same-node pods).
func (f *FlowManager) AddLocalSwitchFlow(localPort string, vni uint) error {
	return f.ofctl("add-flow", f.bridge,
		fmt.Sprintf("table=0,priority=90,in_port=%s,actions=load:%d->NXM_NX_TUN_ID[],normal",
			localPort, vni))
}

// DelFlowsByPort removes all table=0 flows with the given in_port.
func (f *FlowManager) DelFlowsByPort(portName string) error {
	return f.ofctl("del-flows", f.bridge,
		fmt.Sprintf("table=0,in_port=%s", portName))
}

// DelFlowsByVNI removes table=0 flows matching a given tun_id on a geneve port.
func (f *FlowManager) DelFlowsByVNI(vni uint, genevePort string) error {
	return f.ofctl("del-flows", f.bridge,
		fmt.Sprintf("table=0,in_port=%s,tun_id=%d", genevePort, vni))
}
