//go:build linux

package nodeagent

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/nodeagent/ofclient"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// FlowManager programs tables 0 and 6 of the OVS pipeline on br-ovs. The bridge is fail-secure (see
// OVSManager.ensureBridge): table 0 ends in a drop, so only a port with its own t0 flow forwards anything.
//
// v1 pipeline (MAC learning deferred to a future version):
//
//	t0 — normalise in_port → set metadata=VNI, reg0=origin, resubmit(,6)
//	  local port:  reg0=0  (local origin)
//	  Geneve port: reg0=1  (remote origin), move tun_id→metadata
//	t6 — flood by (metadata=VNI, reg0):
//	  reg0=0 (local):  all local ports + Geneve to each remote VTEP
//	  reg0=1 (remote): all local ports only  — prevents Geneve re-flood loops
//
// All t6 entries for a given VNI are rebuilt atomically (delete-all-for-VNI
// then re-add), so reconcileCreate can be called idempotently.
type FlowManager struct {
	client         *ofclient.Client
	proofSocket    string
	nativeFlowRead func(context.Context) ([]nativeFlow, error)

	// geneveSrc are the node addresses (host byte order) whose Geneve traffic is accepted: a packet that arrives on the Geneve
	// port from any other source has no t0 flow and is dropped (R-12). Guarded by gmu.
	gmu       sync.Mutex
	geneveSrc map[uint32]bool

	// The Geneve meters (see geneve_meters.go). bwmu orders the changes of the t6/t7/t8 flows of a VNI and of the meters they use;
	// floods remembers what each VNI was built from, so the flows can be built again when the limits turn on or off.
	bwmu   sync.Mutex
	meters *geneveMeters
	floods map[uint]t6Flood
}

// t6Flood is what RebuildT6Flood was last given for a VNI.
type t6Flood struct {
	localPorts, remoteVTEPs []string
}

func NewFlowManager(ovsRunDir, bridge string) (*FlowManager, error) {
	sockPath := filepath.Join(ovsRunDir, bridge+".mgmt")
	c, err := ofclient.Connect(sockPath)
	if err != nil {
		return nil, fmt.Errorf("OF client connect to %s: %w", sockPath, err)
	}
	log := ctrl.Log.WithName("openflow")
	c.SetErrorHandler(
		func(xid uint32, errType, errCode uint16) {
			// FLOW_MOD is fire-and-forget; a rejected flow (bad OXM field, etc.)
			// would otherwise be invisible while the port stays unbound (its traffic is dropped).
			log.Error(
				fmt.Errorf("OFPT_ERROR type=%d code=%d", errType, errCode),
				"OpenFlow request rejected by OVS", "xid", xid,
			)
		},
	)
	fm := &FlowManager{client: c, proofSocket: sockPath, meters: newGeneveMeters(c), floods: map[uint]t6Flood{}}
	if err := fm.resetPipeline(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return fm, nil
}

// resetPipeline makes the bridge fail-secure from the first moment: every flow of tables 0 and 6 that an earlier
// run (or the standalone default) left behind is removed, and the table-0 default is a drop, so a port without
// its own t0 flow reaches nobody. The flows of the live ports come back from the reconcilers within seconds; until
// then nothing is forwarded (the price of never bridging ports of different teams).
func (f *FlowManager) resetPipeline() error {
	for _, table := range []uint8{0, 6, geneveTable1, geneveTable2} {
		if err := f.client.FlowDeleteTable(table); err != nil {
			return fmt.Errorf("clear table %d: %w", table, err)
		}
	}
	return f.installDefaultDrop()
}

// installDefaultDrop sets table 0, priority 0, to drop. It replaces the NORMAL flow of a standalone bridge (an
// OFPFC_ADD with the same match and priority replaces the flow).
func (f *FlowManager) installDefaultDrop() error {
	if err := f.client.FlowAdd(0, 0, ofclient.BuildMatchAdvanced(0, 0, false, 0, false, 0, false, 0, false), nil); err != nil {
		return fmt.Errorf("install the default drop: %w", err)
	}
	return nil
}

// Close closes the underlying OpenFlow connection.
func (f *FlowManager) Close() error { return f.client.Close() }

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

// refreshPorts re-queries PORT_DESC before an operation that resolves port names.
// OVS recycles ofport numbers when a port is deleted and re-created under the
// same name (veth recovery paths do exactly that), so a cache hit alone can
// return a number that now belongs to a different interface. Every public
// operation refreshes first to program flows against current numbers.
func (f *FlowManager) refreshPorts() error {
	before := f.client.Ports()
	if err := f.client.RefreshPorts(); err != nil {
		return fmt.Errorf("refresh port map: %w", err)
	}
	// A number that was a port and no longer is (or is another port now) must not keep flows: a new port that
	// gets the number would inherit the old VNI (t0, in_port) or receive its flood (t6, output).
	for _, no := range ofclient.StalePortNumbers(before, f.client.Ports()) {
		if err := f.purgePort(no); err != nil {
			return err
		}
	}
	return nil
}

// purgePort removes every flow that matches on or outputs to the OpenFlow port number.
func (f *FlowManager) purgePort(no uint32) error {
	if err := f.client.FlowDeleteStrict(0, 90, ofclient.BuildMatch(no, 0, false)); err != nil {
		return fmt.Errorf("purge t0 of port %d: %w", no, err)
	}
	if err := f.client.FlowDeleteOutPort(no); err != nil {
		return fmt.Errorf("purge flows to port %d: %w", no, err)
	}
	return nil
}

// InitGeneveIngress installs the t0 entries of the shared Geneve port, one per node the cluster has:
//
//	t0, priority=100, in_port=GENEVE, tun_src=NODE → set reg0=1, move tun_id→metadata, resubmit(,6)
//
// Geneve from any other source (UDP 6081 is open to whoever can reach the node) matches no flow and is dropped by the table-0 default,
// so a host that is not a node cannot inject frames into a VNI. The catch-all entry of earlier versions (in_port only) is removed. Safe
// to call multiple times; OFPFC_ADD replaces an existing entry. Must be called after AddGenevePort so the port is visible to OVS. The
// sources are set by SetGeneveSources; until the first call nothing is accepted.
func (f *FlowManager) InitGeneveIngress() error {
	if err := f.refreshPorts(); err != nil {
		return err
	}
	geneveNo, err := f.portNo(GenevePort)
	if err != nil {
		return fmt.Errorf("resolve geneve port: %w", err)
	}
	if err := f.client.FlowDeleteStrict(0, 100, ofclient.BuildMatch(geneveNo, 0, false)); err != nil {
		return fmt.Errorf("remove the catch-all Geneve flow: %w", err)
	}
	f.gmu.Lock()
	defer f.gmu.Unlock()
	for ip := range f.geneveSrc {
		if err := f.addGeneveSource(geneveNo, ip); err != nil {
			return err
		}
	}
	return nil
}

func (f *FlowManager) addGeneveSource(geneveNo, ip uint32) error {
	actions := make([]byte, 0, 128)
	actions = append(actions, ofclient.BuildActionsSetReg0(1)...)
	actions = append(
		actions, ofclient.BuildActionsRegMove(
			64, 0, 0,
			ofclient.OxmIDTunnelID(), ofclient.OxmIDMetadata(),
		)...,
	)
	actions = append(actions, ofclient.BuildActionsResubmitTable(6)...)
	return f.client.FlowAdd(0, 100, ofclient.BuildMatchTunSrc(geneveNo, ip), actions)
}

// SetGeneveSources makes these node addresses the only ones whose Geneve traffic is accepted: a flow is added for each new address
// and removed for each that is gone. Called with the same set it does nothing. It needs the Geneve port to exist (a call before that is
// remembered and applied by InitGeneveIngress).
func (f *FlowManager) SetGeneveSources(ips []net.IP) error {
	want := map[uint32]bool{}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			want[binary.BigEndian.Uint32(v4)] = true
		}
	}
	f.gmu.Lock()
	defer f.gmu.Unlock()
	var add, del []uint32
	for ip := range want {
		if !f.geneveSrc[ip] {
			add = append(add, ip)
		}
	}
	for ip := range f.geneveSrc {
		if !want[ip] {
			del = append(del, ip)
		}
	}
	sort.Slice(add, func(i, j int) bool { return add[i] < add[j] })
	f.geneveSrc = want
	if len(add) == 0 && len(del) == 0 {
		return nil
	}
	if err := f.refreshPorts(); err != nil {
		return err
	}
	geneveNo, err := f.portNo(GenevePort)
	if err != nil {
		return nil // the port is not there yet: InitGeneveIngress installs the flows when it is
	}
	for _, ip := range add {
		if err := f.addGeneveSource(geneveNo, ip); err != nil {
			return err
		}
	}
	for _, ip := range del {
		if err := f.client.FlowDeleteStrict(0, 100, ofclient.BuildMatchTunSrc(geneveNo, ip)); err != nil {
			return fmt.Errorf("remove the Geneve flow of %s: %w", net.IPv4(byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip)), err)
		}
	}
	return nil
}

// AddT0Port installs a t0 entry for a local device or patch port:
//
//	t0, priority=90, in_port=PORT → set reg0=0, load VNI→metadata, resubmit(,6)
//
// Idempotent and atomic: OFPFC_ADD with the same match and priority replaces the flow, so the port is never
// without its t0 flow while it is rebound (a delete first would leave a gap).
func (f *FlowManager) AddT0Port(portName string, vni uint) error {
	if err := f.refreshPorts(); err != nil {
		return err
	}
	portNo, err := f.portNo(portName)
	if err != nil {
		return reconcileutil.Waiting(fmt.Errorf("resolve port %q: %w", portName, err), 0)
	}
	match := ofclient.BuildMatch(portNo, 0, false)
	actions := make([]byte, 0, 128)
	actions = append(actions, ofclient.BuildActionsSetReg0(0)...)
	actions = append(actions, ofclient.BuildActionsSetMetadata(uint64(vni))...)
	actions = append(actions, ofclient.BuildActionsResubmitTable(6)...)
	return f.client.FlowAdd(0, 90, match, actions)
}

// DelT0Port removes the t0 entry for portName.
// No-op if the port is already gone from the port map.
func (f *FlowManager) DelT0Port(portName string) error {
	if err := f.refreshPorts(); err != nil {
		return err
	}
	portNo, err := f.portNo(portName)
	if err != nil {
		return nil // port already absent — nothing to delete
	}
	return f.client.FlowDelete(0, ofclient.BuildMatch(portNo, 0, false))
}

// retirePort confirms removal before the owning veth is deleted and its
// ofport can be recycled. Ownership must be checked before calling this.
func (f *FlowManager) retirePort(name string) error {
	if err := f.refreshPorts(); err != nil {
		return err
	}
	no, err := f.portNo(name)
	if err != nil {
		return reconcileutil.Waiting(fmt.Errorf("port retirement cannot resolve %q: %w", name, err), 0)
	}
	return f.client.RetirePort(no)
}

// RebuildT6Flood atomically replaces the t6 flood entries for vni:
//
//	priority=110, metadata=VNI, reg0=0 → output all localPorts + Geneve to each remoteVTEP
//	priority=100, metadata=VNI, reg0=1 → output all localPorts only
//
// Deletes any previous t6 entries for this VNI first.
// No-op (deletes only) when localPorts is empty.
//
// With the Geneve limits on and a remote VTEP, the Geneve outputs move out of t6 into two more tables, each behind a meter
// (see geneve_meters.go): local delivery stays unmetered, only what leaves the node is counted.
func (f *FlowManager) RebuildT6Flood(vni uint, localPorts, remoteVTEPs []string) error {
	f.bwmu.Lock()
	defer f.bwmu.Unlock()
	f.initBandwidthLocked()
	f.floods[vni] = t6Flood{localPorts: append([]string(nil), localPorts...), remoteVTEPs: append([]string(nil), remoteVTEPs...)}
	return f.rebuildT6Locked(vni)
}

func (f *FlowManager) rebuildT6Locked(vni uint) error {
	fl := f.floods[vni]
	if err := f.delFloodFlowsLocked(vni); err != nil {
		return err
	}
	if len(fl.localPorts) == 0 {
		return f.dropMetersLocked(vni)
	}
	if err := f.refreshPorts(); err != nil {
		return err
	}

	localNos := make([]uint32, 0, len(fl.localPorts))
	for _, p := range fl.localPorts {
		no, err := f.portNo(p)
		if err != nil {
			return fmt.Errorf("resolve local port %q: %w", p, err)
		}
		localNos = append(localNos, no)
	}

	var geneveNo uint32
	if len(fl.remoteVTEPs) > 0 {
		var err error
		if geneveNo, err = f.portNo(GenevePort); err != nil {
			return fmt.Errorf("resolve geneve port: %w", err)
		}
	}
	ves := make([]uint32, 0, len(fl.remoteVTEPs))
	for _, vtep := range fl.remoteVTEPs {
		ipBE, err := ipv4BE(vtep)
		if err != nil {
			return fmt.Errorf("parse VTEP %q: %w", vtep, err)
		}
		ves = append(ves, ipBE)
	}

	limited := len(ves) > 0 && f.meters.Enabled()
	if limited {
		// the meters are there before the flows that name them
		if err := f.meters.Split(vni, len(ves)); err != nil {
			return fmt.Errorf("the Geneve meters of VNI %d: %w", vni, err)
		}
	}
	plan := planT6(vni, localNos, ves, geneveNo, limited)
	for _, flow := range plan {
		var err error
		if flow.checked {
			err = f.client.FlowAddInstr(flow.table, flow.priority, flow.match, flow.ins)
		} else {
			err = f.client.FlowAdd(flow.table, flow.priority, flow.match, flow.ins.Actions)
		}
		if err != nil {
			return fmt.Errorf("add %s VNI %d: %w", flow.what, vni, err)
		}
	}
	if !limited {
		return f.dropMetersLocked(vni)
	}
	return nil
}

// initBandwidthLocked makes the bandwidth state of a FlowManager that was not built by NewFlowManager (the tests build bare ones).
func (f *FlowManager) initBandwidthLocked() {
	if f.floods == nil {
		f.floods = map[uint]t6Flood{}
	}
	if f.meters == nil {
		f.meters = newGeneveMeters(f.client)
	}
}

// dropMetersLocked removes the meter of a VNI that no longer floods to a remote node (its flows are already built without it).
func (f *FlowManager) dropMetersLocked(vni uint) error {
	if f.meters == nil {
		return nil // no limits were ever set on this manager
	}
	if err := f.meters.Unsplit(vni); err != nil {
		return fmt.Errorf("the Geneve meters of VNI %d: %w", vni, err)
	}
	return nil
}

// delFloodFlowsLocked removes the flows of a VNI from t6 and from the meter tables.
func (f *FlowManager) delFloodFlowsLocked(vni uint) error {
	match := ofclient.BuildMatchAdvanced(0, uint64(vni), true, 0, false, 0, false, 0, false)
	for _, table := range []uint8{6, geneveTable1, geneveTable2} {
		if err := f.client.FlowDelete(table, match); err != nil {
			return err
		}
	}
	return nil
}

// DelT6Flood removes all flood entries matching metadata=VNI (both reg0 values, and the meter tables) and the meter of the VNI.
func (f *FlowManager) DelT6Flood(vni uint) error {
	f.bwmu.Lock()
	defer f.bwmu.Unlock()
	delete(f.floods, vni)
	if err := f.delFloodFlowsLocked(vni); err != nil {
		return err
	}
	return f.dropMetersLocked(vni)
}

// SetGeneveLimits gives the node its Geneve ceilings (from the operator's annotation on the node): zero limits turn the meters
// off. A change of the rates is a mod-meter on the meters that changed; turning on or off builds the floods of the split labs
// again, with or without the meters.
func (f *FlowManager) SetGeneveLimits(l bandwidth.Limits) error {
	f.bwmu.Lock()
	defer f.bwmu.Unlock()
	f.initBandwidthLocked()
	toggled, err := f.meters.SetLimits(l)
	if err != nil {
		return err
	}
	if !toggled {
		return nil
	}
	var first error
	for vni, fl := range f.floods {
		if len(fl.remoteVTEPs) == 0 {
			continue
		}
		if err := f.rebuildT6Locked(vni); err != nil && first == nil {
			first = err
		}
	}
	if !f.meters.Enabled() {
		if err := f.meters.Clear(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ipv4BE parses a dotted-quad IPv4 address into a big-endian uint32 for
// NXM_NX_TUN_IPV4_DST / NXM_NX_TUN_IPV4_SRC values.
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

// --- Compatibility shims for call sites not yet migrated ---

// AddEgressFlow replaces the old per-remote-node egress rule.
// Now just binds the local port to its VNI via t0; Geneve flood is handled by
// RebuildT6Flood called from the connection reconciler.
func (f *FlowManager) AddEgressFlow(localPort, _ string, vni uint, _ string) error {
	return f.AddT0Port(localPort, vni)
}

// AddIngressFlow is a no-op in the new pipeline.
// The single Geneve t0 entry installed by InitGeneveIngress handles all ingress.
func (f *FlowManager) AddIngressFlow(_, _ string, _ uint) error { return nil }

// AddLocalSwitchFlow binds a local port to its VNI via t0 (same as AddT0Port).
func (f *FlowManager) AddLocalSwitchFlow(localPort string, vni uint) error {
	return f.AddT0Port(localPort, vni)
}

// DelFlowsByPort removes the t0 entry for portName.
func (f *FlowManager) DelFlowsByPort(portName string) error { return f.DelT0Port(portName) }

// DelFlowsByVNI removes all t6 flood entries for vni.
func (f *FlowManager) DelFlowsByVNI(vni uint, _ string) error { return f.DelT6Flood(vni) }

// RetireVNI removes every flow of the VNI in every table, confirmed, and then the Geneve meter of the lab (the flows that used it
// are gone by then).
func (f *FlowManager) RetireVNI(vni uint) error {
	f.bwmu.Lock()
	defer f.bwmu.Unlock()
	delete(f.floods, vni)
	if err := f.client.RetireVNI(uint64(vni)); err != nil {
		return err
	}
	return f.dropMetersLocked(vni)
}
