//go:build linux

package nodeagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/types"
)

// portKey computes a stable OVS port name (max 15 chars) for a device interface.
func portKey(namespace, connection, iface string) string {
	h := sha256.Sum256([]byte(namespace + "/" + connection + "/" + iface))
	return fmt.Sprintf("p%x", h[:4])
}

// GenevePort is the single per-node Geneve VTEP (spec §7: "One Geneve port
// per node: options:remote_ip=flow, options:key=flow. The remote VTEP address
// and VNI are set per-flow"). All cross-node tunnels share this port; flow
// rules set tun_dst (NXM_NX_TUN_IPV4_DST) and tun_id per packet.
const GenevePort = "ovsgnv0"

// genevePortName is retained for transitional call sites and returns the
// shared port name regardless of the remote address argument.
// OVSManager programs the single br-ovs bridge via libovsdb (OVSDB JSON-RPC over Unix socket).
type OVSManager struct {
	// RuntimeRetirement persists first exact physical cleanup before row loss.
	RuntimeRetirement       func(string, types.UID, string) error
	RuntimePrepare          func(string, types.UID, string) error
	FabricPrepare           func(string, types.UID, string) error
	FabricRetirement        func(string, types.UID, string) error
	FabricRetirementAbsent  func(string, types.UID) error
	RuntimeRetirementAbsent func(string, types.UID) error
	bridge                  string
	client                  client.Client
	ctx                     context.Context
	mu                      sync.Mutex
	vethMu                  sync.Mutex // serialize Pod netns wiring against replacement cleanup

	// policingKbps is the rate a veth port of a device may send into the bridge (0 = unpoliced); set by SetPolicing.
	policingKbps int
}

// SetPolicing sets the storm control of the veth ports: each may send at most kbps into the bridge (burst a tenth of it). A pure flood
// or a loop through a Linux bridge inside one lab is held to that rate instead of the whole datapath of the node. 0 switches it off.
func (m *OVSManager) SetPolicing(kbps int) {
	m.mu.Lock()
	m.policingKbps = kbps
	m.mu.Unlock()
}

// Ping asks ovsdb-server for an answer: the node-agent has lost it when this fails.
func (m *OVSManager) Ping(ctx context.Context) error { return m.client.Echo(ctx) }

// EnsurePolicing applies the policing rate to an existing veth port (one made before the setting, or under another value).
func (m *OVSManager) EnsurePolicing(portName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensurePolicingLocked(portName)
}

func (m *OVSManager) ensurePolicingLocked(portName string) error {
	kbps := m.policingKbps
	ifaces := []OVSInterface{}
	if err := m.client.List(m.ctx, &ifaces); err != nil {
		return fmt.Errorf("list interfaces: %w", err)
	}
	for i := range ifaces {
		if ifaces[i].Name != portName {
			continue
		}
		burst := kbps / 10
		if ifaces[i].IngressPolicingRate == kbps && ifaces[i].IngressPolicingBurst == burst {
			return nil
		}
		ifaces[i].IngressPolicingRate, ifaces[i].IngressPolicingBurst = kbps, burst
		ops, err := m.client.Where(&ifaces[i]).Update(&ifaces[i], &ifaces[i].IngressPolicingRate, &ifaces[i].IngressPolicingBurst)
		if err != nil {
			return fmt.Errorf("police %q: %w", portName, err)
		}
		results, err := m.client.Transact(m.ctx, ops...)
		if err != nil {
			return fmt.Errorf("transact policing of %q: %w", portName, err)
		}
		if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
			return fmt.Errorf("policing of %q result: %w", portName, err)
		}
		return nil
	}
	return nil // the port is not there (any more)
}

func NewOVSManager(bridge, sockPath string) (*OVSManager, error) {
	dbModel, err := model.NewClientDBModel(
		"Open_vSwitch", map[string]model.Model{
			"Open_vSwitch": &OVSOpen_vSwitch{},
			"Bridge":       &OVSBridge{},
			"Port":         &OVSPort{},
			"Interface":    &OVSInterface{},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("build OVSDB model: %w", err)
	}

	ovs, err := client.NewOVSDBClient(
		dbModel,
		client.WithEndpoint("unix:"+sockPath),
		client.WithLeaderOnly(false),
	)
	if err != nil {
		return nil, fmt.Errorf("create OVSDB client: %w", err)
	}

	ctx := context.Background()
	// Retry connect — ovsdb-server may still be starting.
	var connErr error
	for i := 0; i < 30; i++ {
		if connErr = ovs.Connect(ctx); connErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if connErr != nil {
		return nil, fmt.Errorf("connect to OVSDB %s: %w", sockPath, connErr)
	}

	// Monitor only modeled columns. A vswitchd statistics update otherwise
	// includes an unknown column and libovsdb drops the same update's ifindex
	// and policing changes, leaving the node-agent's cache stale.
	root, br, port, iface := &OVSOpen_vSwitch{}, &OVSBridge{}, &OVSPort{}, &OVSInterface{}
	monitor := ovs.NewMonitor(
		client.WithTable(root, &root.Bridges),
		client.WithTable(br, &br.Name, &br.Ports, &br.FailMode),
		client.WithTable(port, &port.Name, &port.Interfaces, &port.ExternalIDs),
		client.WithTable(iface, &iface.Name, &iface.Type, &iface.Options, &iface.Ifindex,
			&iface.IngressPolicingRate, &iface.IngressPolicingBurst),
	)
	if _, err := ovs.Monitor(ctx, monitor); err != nil {
		return nil, fmt.Errorf("OVSDB monitor: %w", err)
	}

	m := &OVSManager{bridge: bridge, client: ovs, ctx: ctx}
	return m, m.ensureBridge()
}

func (m *OVSManager) findBridge() (*OVSBridge, error) {
	bridges := []OVSBridge{}
	if err := m.client.List(m.ctx, &bridges); err != nil {
		return nil, fmt.Errorf("list bridges: %w", err)
	}
	for i := range bridges {
		if bridges[i].Name == m.bridge {
			return &bridges[i], nil
		}
	}
	return nil, nil
}

// FailModeSecure is the only fail mode of br-ovs: no flow, no forwarding.
const FailModeSecure = "secure"

// ensureSecure puts an existing bridge (made by an earlier version in the standalone mode) into the secure
// fail mode.
func (m *OVSManager) ensureSecure(br *OVSBridge) error {
	if br.FailMode != nil && *br.FailMode == FailModeSecure {
		return nil
	}
	secure := FailModeSecure
	br.FailMode = &secure
	ops, err := m.client.Where(br).Update(br, &br.FailMode)
	if err != nil {
		return fmt.Errorf("set fail_mode op: %w", err)
	}
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact fail_mode: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("fail_mode result: %w", err)
	}
	return nil
}

// ensureBridge is only called from NewOVSManager before the client is shared — no mutex needed.
func (m *OVSManager) ensureBridge() error {
	if br, err := m.findBridge(); err != nil {
		return err
	} else if br != nil {
		return m.ensureSecure(br)
	}

	secure := FailModeSecure
	bridgeNamedUUID := "bridge_new"
	bridge := OVSBridge{UUID: bridgeNamedUUID, Name: m.bridge, FailMode: &secure}
	bridgeOps, err := m.client.Create(&bridge)
	if err != nil {
		return fmt.Errorf("create bridge op: %w", err)
	}

	// Mutate Open_vSwitch root row to add bridge reference.
	roots := []OVSOpen_vSwitch{}
	if err := m.client.List(m.ctx, &roots); err != nil {
		return fmt.Errorf("list Open_vSwitch root: %w", err)
	}
	if len(roots) == 0 {
		return fmt.Errorf("Open_vSwitch root row not found")
	}
	mutOps, err := m.client.Where(&roots[0]).Mutate(
		&roots[0],
		model.Mutation{
			Field:   &roots[0].Bridges,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{bridgeNamedUUID},
		},
	)
	if err != nil {
		return fmt.Errorf("mutate root bridges: %w", err)
	}

	ops := append(bridgeOps, mutOps...)
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact ensureBridge: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("ensureBridge result: %w", err)
	}
	return nil
}

// VethPeerName returns the pod-side name of the veth pair for a given host-side (stableKey).
// Always ≤15 chars (Linux IFNAMSIZ limit). Falls back to "v"+sha256[:6] when the naive
// "v"+stableKey would exceed the limit.
func VethPeerName(stableKey string) string {
	if peer := "v" + stableKey; len(peer) <= 15 {
		return peer
	}
	h := sha256.Sum256([]byte(stableKey))
	return fmt.Sprintf("v%x", h[:6]) // 13 chars, collision-free
}

// AddVethPort creates a veth pair where stableKey is the host-side name (stays in OVS/root
// netns) and VethPeerName(stableKey) is the pod-side (moved to pod netns by the caller).
// Idempotent: EEXIST on kernel side is ignored; port already in OVS is a no-op.
// Stores stableKey in external_ids so FindPortByKey can locate it.
func (m *OVSManager) AddVethPort(stableKey string) error {
	return m.addVethPort(stableKey, "")
}

// AddVethPortOwned never overwrites another Pod's stable port ownership.
func (m *OVSManager) AddVethPortOwned(stableKey string, ownerUID types.UID) error {
	if ownerUID == "" {
		return fmt.Errorf("%w: empty Pod UID", ErrPortOwnerUnknown)
	}
	return m.addVethPort(stableKey, ownerUID)
}

func (m *OVSManager) addVethPort(stableKey string, ownerUID types.UID) error {
	if !ValidPortKey(stableKey) {
		return fmt.Errorf("%q is not a port key of the platform: refusing to create a veth under it", stableKey)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.portSnapshotLocked(stableKey)
	if err != nil {
		return err
	}
	if p == nil && ownerUID != "" {
		if _, err := netlink.LinkByName(stableKey); err == nil {
			return fmt.Errorf("%w: existing kernel link %s has no OVS owner", ErrPortOwnerUnknown, stableKey)
		} else {
			var absent netlink.LinkNotFoundError
			if !errors.As(err, &absent) {
				return err
			}
		}
	}
	if p != nil && types.UID(p.ExternalIDs[portOwnerExternalID]) != ownerUID {
		return fmt.Errorf("%w: %s belongs to %q, requested %q", ErrPortOwnerChanged, stableKey, p.ExternalIDs[portOwnerExternalID], ownerUID)
	}

	podSide := VethPeerName(stableKey)
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: stableKey},
		PeerName:  podSide,
	}
	if err := netlink.LinkAdd(veth); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create veth %q/%q: %w", stableKey, podSide, err)
	}
	// Bring host-side up so OVS can use it.
	link, err := netlink.LinkByName(stableKey)
	if err != nil {
		return fmt.Errorf("get host-side veth %q: %w", stableKey, err)
	}
	if err := requireVeth(link); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set up host-side veth %q: %w", stableKey, err)
	}
	ids := map[string]string{portKeyExternalID: stableKey}
	if ownerUID != "" {
		ids[portOwnerExternalID] = string(ownerUID)
	}
	err = m.addPort(stableKey, "system", nil, ids)
	policed := m.policingKbps > 0
	if err != nil {
		return err
	}
	if policed {
		return m.ensurePolicingLocked(stableKey)
	}
	return nil
}

// portKeyRE is the shape of every port key the platform makes (names.DevicePortKey, VPNHostPortKey, GWHostPortKey): a letter and 12
// hex digits. A key of any other shape is never a veth of ours: it could be "eth0".
var portKeyRE = regexp.MustCompile(`^[gnp][0-9a-f]{12}$`)

// ValidPortKey says whether key may be used as the name of a veth the node-agent creates or deletes.
func ValidPortKey(key string) bool { return portKeyRE.MatchString(key) }

// requireVeth refuses to touch an existing link that is not a veth: a host interface that happens to carry a name.
func requireVeth(link netlink.Link) error {
	if link.Type() != "veth" {
		return fmt.Errorf("%q is a %s, not a veth of the node-agent: left alone", link.Attrs().Name, link.Type())
	}
	return nil
}

// DelVethPort removes the OVS port and deletes the kernel veth pair for stableKey.
// Deleting the host-side also removes the pod-side (veth pair invariant).
// Idempotent: no-op if already gone.
func (m *OVSManager) DelVethPort(stableKey string) error {
	if !ValidPortKey(stableKey) {
		return fmt.Errorf("%q is not a port key of the platform: refusing to delete a link under it", stableKey)
	}
	if err := m.DelPort(stableKey); err != nil {
		return err
	}
	link, err := netlink.LinkByName(stableKey)
	if err != nil {
		return nil // already gone
	}
	if err := requireVeth(link); err != nil {
		return err
	}
	return netlink.LinkDel(link)
}

// portKeyExternalID is the external_ids key used to store the stable port key.
const portKeyExternalID = "port-key"

// The exact Pod UID is the Pod incarnation; OVS Port UUID fences row replacement.
const portOwnerExternalID = "pod-uid"

var ErrPortOwnerUnknown = errors.New("port ownership unknown")
var ErrPortOwnerChanged = errors.New("port belongs to another Pod incarnation")

// portSnapshotLocked reads the database, not a possibly delayed monitor cache.
func (m *OVSManager) portSnapshotLocked(key string) (*OVSPort, error) {
	keyIDs, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key})
	ops := []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: "Port", Where: []ovsdb.Condition{ovsdb.NewCondition("external_ids", ovsdb.ConditionIncludes, keyIDs)}, Columns: []string{"_uuid", "name", "external_ids"}}}
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return nil, err
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return nil, err
	}
	if len(results[0].Rows) == 0 {
		return nil, nil
	}
	if len(results[0].Rows) != 1 || results[0].Rows[0]["name"] != key {
		return nil, fmt.Errorf("%w: ambiguous/non-veth stable key %s", ErrPortOwnerUnknown, key)
	}
	row := results[0].Rows[0]
	ids := map[string]string{}
	if value, ok := row["external_ids"].(ovsdb.OvsMap); ok {
		for k, v := range value.GoMap {
			ids[k.(string)] = v.(string)
		}
	}
	uuid, ok := row["_uuid"].(ovsdb.UUID)
	if !ok || ids[portKeyExternalID] != key {
		return nil, fmt.Errorf("%w: %s", ErrPortOwnerUnknown, key)
	}
	return &OVSPort{UUID: uuid.GoUUID, Name: key, ExternalIDs: ids}, nil
}

func (m *OVSManager) PortOwners() (map[string]types.UID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.portOwnersLocked()
}
func (m *OVSManager) portOwnersLocked() (map[string]types.UID, error) {
	ops := []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: "Port", Where: []ovsdb.Condition{}, Columns: []string{"external_ids"}}}
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return nil, err
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return nil, err
	}
	owners := map[string]types.UID{}
	for _, row := range results[0].Rows {
		ids, ok := row["external_ids"].(ovsdb.OvsMap)
		if !ok {
			continue
		}
		key, _ := ids.GoMap[portKeyExternalID].(string)
		uid, _ := ids.GoMap[portOwnerExternalID].(string)
		if key != "" {
			if _, present := owners[key]; present {
				return nil, fmt.Errorf("%w: duplicate stable key %s", ErrPortOwnerUnknown, key)
			}
			owners[key] = types.UID(uid)
		}
	}
	return owners, nil
}

// delVethWithFlowsOwned holds the same lock as creation for owner check, flow
// retirement and deletion. Empty UID is allowed only by the caller's legacy fence.
func (m *OVSManager) delVethWithFlowsOwned(key string, uid types.UID, flows *FlowManager) error {
	return m.delVethWithFlowsOwnedJournaled(key, uid, flows, nil)
}

func (m *OVSManager) delVethWithFlowsOwnedJournaled(key string, uid types.UID, flows *FlowManager, prepared func(string) error) error {
	if !ValidPortKey(key) {
		return fmt.Errorf("invalid platform port key %q", key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prepared == nil && flows != nil && m.RuntimeRetirement != nil {
		prepared = func(row string) error { return m.RuntimeRetirement(key, uid, row) }
	}
	p, err := m.portSnapshotLocked(key)
	if err != nil {
		return err
	}
	link, linkErr := netlink.LinkByName(key)
	if p == nil {
		if flows != nil {
			var absent netlink.LinkNotFoundError
			if errors.As(linkErr, &absent) && m.RuntimeRetirementAbsent != nil {
				return m.RuntimeRetirementAbsent(key, uid)
			}
			return fmt.Errorf("%w: missing owner row cannot authorize cleanup of %s", ErrPortOwnerUnknown, key)
		}
		if linkErr == nil {
			return fmt.Errorf("%w: kernel link %s has no OVS owner", ErrPortOwnerUnknown, key)
		}
		var absent netlink.LinkNotFoundError
		if !errors.As(linkErr, &absent) {
			return linkErr
		}
		return nil
	}
	if types.UID(p.ExternalIDs[portOwnerExternalID]) != uid {
		return fmt.Errorf("%w: %s", ErrPortOwnerChanged, key)
	}
	if linkErr == nil {
		if err := requireVeth(link); err != nil {
			return err
		}
	}
	if flows != nil && m.RuntimePrepare != nil {
		if err := m.RuntimePrepare(key, uid, p.UUID); err != nil {
			return err
		}
	}
	if flows != nil {
		if err := flows.retirePort(key); err != nil {
			return err
		}
	}
	current, err := m.portSnapshotLocked(key)
	if err != nil {
		return err
	}
	if current == nil || current.UUID != p.UUID || current.ExternalIDs[portOwnerExternalID] != p.ExternalIDs[portOwnerExternalID] {
		return fmt.Errorf("%w: port changed during flow retirement", ErrPortOwnerChanged)
	}
	if prepared != nil {
		// Keep the exact owner row until the physical flow/kernel proof is fsynced.
		if linkErr == nil {
			currentLink, e := netlink.LinkByName(key)
			if e != nil {
				return e
			}
			if currentLink.Attrs().Index != link.Attrs().Index {
				return ErrPortOwnerChanged
			}
			if e = netlink.LinkDel(currentLink); e != nil {
				return e
			}
		} else {
			var absent netlink.LinkNotFoundError
			if !errors.As(linkErr, &absent) {
				return linkErr
			}
		}
		if e := prepared(p.UUID); e != nil {
			return e
		}
		return m.delPortLocked(p)
	}
	if err := m.delPortLocked(p); err != nil {
		return err
	}
	if linkErr != nil {
		var absent netlink.LinkNotFoundError
		if !errors.As(linkErr, &absent) {
			return linkErr
		}
		return nil
	}
	currentLink, err := netlink.LinkByName(key)
	if err != nil {
		var absent netlink.LinkNotFoundError
		if errors.As(err, &absent) {
			return nil
		}
		return err
	}
	if currentLink.Attrs().Index != link.Attrs().Index {
		return fmt.Errorf("%w: kernel link incarnation changed", ErrPortOwnerChanged)
	}
	return netlink.LinkDel(link)
}

// randomPortName generates a unique OVS internal port name: "ice" + 12 random hex chars = 15 chars (IFNAMSIZ max).
// The name is used only as the kernel interface name; the stable port key is stored in external_ids.
func randomPortName() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random port name: %w", err)
	}
	return fmt.Sprintf("ice%x", b), nil
}

// AddInternalPort creates an OVS internal port in br-ovs with a random kernel interface name.
// The stableKey (used for reconcile idempotency and cleanup) is stored in external_ids["port-key"].
// Returns the random kernel interface name that will appear in the host netns.
func (m *OVSManager) AddInternalPort(stableKey string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, err := randomPortName()
	if err != nil {
		return "", err
	}
	return name, m.addPort(name, "internal", nil, map[string]string{portKeyExternalID: stableKey})
}

// FindPortByKey returns the OVS port name whose external_ids["port-key"] matches stableKey.
// Returns ("", false, nil) if not found.
func (m *OVSManager) FindPortByKey(stableKey string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.findPortByKey(stableKey)
	if err != nil || p == nil {
		return "", false, err
	}
	return p.Name, true, nil
}

func (m *OVSManager) findPortByKey(stableKey string) (*OVSPort, error) {
	ports := []OVSPort{}
	if err := m.client.List(m.ctx, &ports); err != nil {
		return nil, fmt.Errorf("list ports: %w", err)
	}
	for i := range ports {
		if ports[i].ExternalIDs[portKeyExternalID] == stableKey {
			return &ports[i], nil
		}
	}
	return nil, nil
}

// PortKeys returns the stable keys of the OVS ports the platform made (external_ids["port-key"]), as a set.
func (m *OVSManager) PortKeys() (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ports := []OVSPort{}
	if err := m.client.List(m.ctx, &ports); err != nil {
		return nil, fmt.Errorf("list ports: %w", err)
	}
	keys := map[string]bool{}
	for i := range ports {
		if k := ports[i].ExternalIDs[portKeyExternalID]; k != "" {
			keys[k] = true
		}
	}
	return keys, nil
}

// DelPortByKey removes the OVS port whose external_ids["port-key"] matches stableKey.
// Idempotent: no-op if not found.
func (m *OVSManager) DelPortByKey(stableKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.findPortByKey(stableKey)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	return m.delPortLocked(p)
}

// AddGenevePort creates the single shared Geneve VTEP. The arguments are
// retained for source compatibility with older call sites and ignored; the
// concrete VTEP target is driven from flow rules (NXM_NX_TUN_IPV4_DST) so
// only one port is needed per node.
func (m *OVSManager) AddGenevePort(_, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addPort(
		GenevePort, "geneve", map[string]string{
			"remote_ip": "flow",
			"key":       "flow",
		}, nil,
	)
}

// AddPatchPair creates two paired patch ports — spec §7-§8: switch↔switch
// connections materialise as a patch-pair (each end is a port in its own
// switch's VNI; no third VNI is introduced). Patch is always intra-node;
// cross-node hops travel via the partner switch's Geneve mesh.
//
// Idempotent: re-creating an existing pair is a no-op.
func (m *OVSManager) AddPatchPair(nameA, nameB string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.addPort(nameA, "patch", map[string]string{"peer": nameB}, nil); err != nil {
		return err
	}
	return m.addPort(nameB, "patch", map[string]string{"peer": nameA}, nil)
}

// patchPortName computes a stable OVS patch-end name for one end of a switch↔switch link, keyed by the namespace, the connection name
// and the switch device: two groups can have a connection of the same name (lab ids are chosen by the caller), and the name must not
// be shared. 14 chars ("pt" and 12 hex), inside Linux IFNAMSIZ.
func patchPortName(namespace, connName, switchDevice string) string {
	h := sha256.Sum256([]byte(namespace + "/" + connName + "/" + switchDevice))
	return fmt.Sprintf("pt%x", h[:6])
}

// legacyPatchPortName is the name earlier versions gave: without the namespace, so two groups could share a port and one delete the
// other's. A port under it is removed when its connection is reconciled (see ConnectionReconciler).
func legacyPatchPortName(connName, switchDevice string) string {
	h := sha256.Sum256([]byte(connName + "/" + switchDevice))
	return fmt.Sprintf("pt%x", h[:5])
}

// findPort returns the first Port matching by name, or nil if not found.
func (m *OVSManager) findPort(name string) (*OVSPort, error) {
	ports := []OVSPort{}
	if err := m.client.List(m.ctx, &ports); err != nil {
		return nil, fmt.Errorf("list ports: %w", err)
	}
	for i := range ports {
		if ports[i].Name == name {
			return &ports[i], nil
		}
	}
	return nil, nil
}

func (m *OVSManager) addPort(name, ifaceType string, options, externalIDs map[string]string) error {
	// Veth ownership checks use the database; other port kinds retain cache idempotency.
	var p *OVSPort
	var err error
	if externalIDs[portKeyExternalID] == name && ValidPortKey(name) {
		p, err = m.portSnapshotLocked(name)
	} else {
		p, err = m.findPort(name)
	}
	if err != nil {
		return err
	} else if p != nil {
		return nil
	}

	br, err := m.findBridge()
	if err != nil {
		return err
	}
	if br == nil {
		return fmt.Errorf("bridge %q not found", m.bridge)
	}

	ifaceNamedUUID := "iface_new"
	portNamedUUID := "port_new"

	iface := OVSInterface{UUID: ifaceNamedUUID, Name: name, Type: ifaceType, Options: options}
	ifaceOps, err := m.client.Create(&iface)
	if err != nil {
		return fmt.Errorf("create interface op: %w", err)
	}

	port := OVSPort{UUID: portNamedUUID, Name: name, Interfaces: []string{ifaceNamedUUID}, ExternalIDs: externalIDs}
	portOps, err := m.client.Create(&port)
	if err != nil {
		return fmt.Errorf("create port op: %w", err)
	}

	mutOps, err := m.client.Where(br).Mutate(
		br,
		model.Mutation{
			Field:   &br.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{portNamedUUID},
		},
	)
	if err != nil {
		return fmt.Errorf("mutate bridge ports: %w", err)
	}

	ops := append(ifaceOps, append(portOps, mutOps...)...)
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact addPort %q: %w", name, err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("addPort %q result: %w", name, err)
	}
	return nil
}

// DelPort removes a port from br-ovs by name (idempotent).
func (m *OVSManager) DelPort(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.findPort(name)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	return m.delPortLocked(p)
}

func (m *OVSManager) delPortLocked(p *OVSPort) error {
	br, err := m.findBridge()
	if err != nil {
		return err
	}

	var ops []ovsdb.Operation
	if p.ExternalIDs[portKeyExternalID] == p.Name && ValidPortKey(p.Name) {
		ids, _ := ovsdb.NewOvsMap(p.ExternalIDs)
		timeout := 0
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationWait, Table: "Port", Where: []ovsdb.Condition{ovsdb.NewCondition("name", ovsdb.ConditionEqual, p.Name)}, Columns: []string{"_uuid", "external_ids"}, Rows: []ovsdb.Row{{"_uuid": ovsdb.UUID{GoUUID: p.UUID}, "external_ids": ids}}, Until: "==", Timeout: &timeout})
	}
	if br != nil {
		mutOps, err := m.client.Where(br).Mutate(
			br,
			model.Mutation{
				Field:   &br.Ports,
				Mutator: ovsdb.MutateOperationDelete,
				Value:   []string{p.UUID},
			},
		)
		if err != nil {
			return fmt.Errorf("mutate bridge ports: %w", err)
		}
		ops = append(ops, mutOps...)
	}

	delOps, err := m.client.Where(p).Delete()
	if err != nil {
		return fmt.Errorf("delete port op: %w", err)
	}
	ops = append(ops, delOps...)

	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return fmt.Errorf("transact delPort %q: %w", p.Name, err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return fmt.Errorf("delPort %q result: %w", p.Name, err)
	}
	return nil
}

// WaitForPortSetup polls netlink until the kernel interface portName has type "openvswitch"
// with a stable ifindex for at least 300ms.
//
// Why ifindex stability matters: vswitchd may delete+recreate the vport during its initial
// dpif_port_add setup when it detects the interface left root netns (OVS_VPORT_CMD_DEL +
// OVS_VPORT_CMD_NEW in rapid succession, logged as "deleted→added" in vswitchd). The
// recreated interface has a different ifindex. Waiting for ifindex stability ensures we move
// the interface only after vswitchd has entered its "Phase 2" (established) state, where a
// netns move causes it to set ifindex=0 rather than trigger another delete+recreate.
func (m *OVSManager) WaitForPortSetup(portName string, timeout time.Duration) error {
	const stableDuration = 300 * time.Millisecond
	deadline := time.Now().Add(timeout)
	var stableIdx int
	var stableSince time.Time
	for {
		link, err := netlink.LinkByName(portName)
		if err == nil && link.Type() == "openvswitch" {
			idx := link.Attrs().Index
			if idx != stableIdx {
				stableIdx = idx
				stableSince = time.Now()
			} else if time.Since(stableSince) >= stableDuration {
				return nil
			}
		} else {
			stableIdx = 0
			stableSince = time.Time{}
		}
		if time.Now().After(deadline) {
			linkType := ""
			if link != nil {
				linkType = link.Type()
			}
			return fmt.Errorf(
				"interface %q did not stabilize as openvswitch type within %s (type=%q, err=%v)",
				portName,
				timeout,
				linkType,
				err,
			)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Close disconnects from the OVSDB server.
func (m *OVSManager) Close() {
	m.client.Close()
}

// PortExists checks whether a named port exists on br-ovs.
func (m *OVSManager) PortExists(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.findPort(name)
	if err != nil {
		return false, err
	}
	return p != nil, nil
}

const fabricOwnerExternalID = "cice-fabric-owner-uid"

func (m *OVSManager) AddPatchPairOwned(a, b string, owner types.UID) error {
	if owner == "" {
		return ErrPortOwnerUnknown
	}
	m.vethMu.Lock()
	defer m.vethMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range []struct{ name, peer string }{{a, b}, {b, a}} {
		existing, err := m.findPort(entry.name)
		if err != nil {
			return err
		}
		if existing != nil && existing.ExternalIDs[fabricOwnerExternalID] != string(owner) {
			return ErrPortOwnerChanged
		}
		if err := m.addPort(entry.name, "patch", map[string]string{"peer": entry.peer}, map[string]string{fabricOwnerExternalID: string(owner)}); err != nil {
			return err
		}
	}
	return nil
}
func (m *OVSManager) DelFabricPortOwned(key string, owner types.UID, flows *FlowManager) error {
	if owner == "" || flows == nil {
		return ErrPortOwnerUnknown
	}
	m.vethMu.Lock()
	defer m.vethMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	row, err := m.findPort(key)
	if err != nil {
		return err
	}
	if row == nil {
		if m.FabricRetirementAbsent != nil {
			return m.FabricRetirementAbsent(key, owner)
		}
		return ErrPortOwnerUnknown
	}
	if row.ExternalIDs[fabricOwnerExternalID] != string(owner) {
		return ErrPortOwnerChanged
	}
	if m.FabricPrepare == nil || m.FabricRetirement == nil {
		return ErrPortOwnerUnknown
	}
	if err := m.FabricPrepare(key, owner, row.UUID); err != nil {
		return err
	}
	if err := flows.retirePort(key); err != nil {
		return err
	}
	current, err := m.findPort(key)
	if err != nil {
		return err
	}
	if current == nil || current.UUID != row.UUID || current.ExternalIDs[fabricOwnerExternalID] != string(owner) {
		return ErrPortOwnerChanged
	}
	if err := m.FabricRetirement(key, owner, row.UUID); err != nil {
		return err
	}
	return m.delPortLocked(row)
}
