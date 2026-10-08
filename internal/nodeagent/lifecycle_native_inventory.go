//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
	"strings"
)

type nativeFlow struct {
	InPort uint32
	VNI    uint64
	Raw    string
}

var nativeInPort = regexp.MustCompile(`(?:^|,)in_port=([0-9]+)(?:,| )`)
var nativeMetadata = regexp.MustCompile(`(?:metadata=)(0x[0-9a-fA-F]+|[0-9]+)(?:/[^, ]+)?|(?:load:|set_field:)(0x[0-9a-fA-F]+|[0-9]+)->(?:metadata|OXM_OF_METADATA(?:\[[^\]]*\])?)`)
var nativeMetadataMask = regexp.MustCompile(`metadata=(?:0x[0-9a-fA-F]+|[0-9]+)/([^, ]+)`)

func parseNativeFlows(raw string) ([]nativeFlow, error) {
	var flows []nativeFlow
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, "actions=") {
			continue
		}
		flow := nativeFlow{Raw: line}
		for _, mask := range nativeMetadataMask.FindAllStringSubmatch(line, -1) {
			value, err := strconv.ParseUint(mask[1], 0, 64)
			if err != nil || value != ^uint64(0) {
				return nil, fmt.Errorf("partial native VNI match is not an exact domain")
			}
		}
		if m := nativeInPort.FindStringSubmatch(line); len(m) > 0 {
			n, err := strconv.ParseUint(m[1], 10, 32)
			if err != nil {
				return nil, err
			}
			flow.InPort = uint32(n)
		}
		// A local t0 metadata load or a t6 metadata match identifies the VNI domain.
		for _, m := range nativeMetadata.FindAllStringSubmatch(line, -1) {
			n, err := strconv.ParseUint(func() string {
				if m[1] != "" {
					return m[1]
				}
				return m[2]
			}(), 0, 64)
			if err != nil {
				return nil, err
			}
			if flow.VNI != 0 && flow.VNI != n {
				return nil, fmt.Errorf("ambiguous native flow domain")
			}
			flow.VNI = n
		}
		flows = append(flows, flow)
	}
	return flows, nil
}
func (f *FlowManager) readNativeFlows(ctx context.Context) ([]nativeFlow, error) {
	if f == nil {
		return nil, ErrPortOwnerUnknown
	}
	if f.nativeFlowRead != nil {
		return f.nativeFlowRead(ctx)
	}
	if f.proofSocket == "" {
		return nil, fmt.Errorf("native flow endpoint unavailable")
	}
	out, err := exec.CommandContext(ctx, "ovs-ofctl", "-O", "OpenFlow13", "--no-stats", "dump-flows", "unix:"+f.proofSocket).Output()
	if err != nil {
		return nil, fmt.Errorf("native flow enumeration failed: %w", err)
	}
	return parseNativeFlows(string(out))
}
func (o *NativeRuntimeObserver) nativePodInventories() ([]lab.OwnedRuntimeIdentity, error) {
	files, err := os.ReadDir(o.JournalDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []lab.OwnedRuntimeIdentity
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(o.JournalDir, file.Name()))
		if err != nil {
			return nil, err
		}
		if len(b) > 1<<20 {
			return nil, fmt.Errorf("native owner inventory too large")
		}
		var row lab.OwnedRuntimeIdentity
		if json.Unmarshal(b, &row) != nil || row.PodUID == "" || row.OwnerUID == "" || row.NodeName != o.NodeName || row.NodeBootID != o.BootID {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func scopeOwnsPod(scope, row lab.OwnedRuntimeIdentity) bool {
	if row.Namespace != scope.Namespace {
		return false
	}
	if scope.ScopeKind == "NeverMaterialized" {
		return row.OwnerUID == scope.OwnerUID && row.ScopeUID == scope.ScopeUID
	}
	return row.OwnerUID == scope.OwnerUID
}
func mergeNativeScopeIdentity(dst *lab.OwnedRuntimeIdentity, src lab.OwnedRuntimeIdentity) {
	for _, v := range src.FabricPorts {
		found := false
		for _, old := range dst.FabricPorts {
			found = found || old == v
		}
		if !found {
			dst.FabricPorts = append(dst.FabricPorts, v)
		}
	}
	for _, v := range src.VNIBindings {
		found := false
		for _, old := range dst.VNIBindings {
			found = found || old == v
		}
		if !found {
			dst.VNIBindings = append(dst.VNIBindings, v)
		}
	}
	for _, v := range src.VNIs {
		found := false
		for _, old := range dst.VNIs {
			found = found || old == v
		}
		if !found {
			dst.VNIs = append(dst.VNIs, v)
		}
	}
	for _, v := range src.ContainerIDs {
		if !runtimeContainsID(dst.ContainerIDs, v) {
			dst.ContainerIDs = append(dst.ContainerIDs, v)
		}
	}
	for _, v := range src.CgroupPaths {
		if !runtimeContainsID(dst.CgroupPaths, v) {
			dst.CgroupPaths = append(dst.CgroupPaths, v)
		}
	}
	for _, v := range src.PortKeys {
		if !runtimeContainsID(dst.PortKeys, v) {
			dst.PortKeys = append(dst.PortKeys, v)
		}
	}
	for _, v := range src.PortRows {
		found := false
		for _, old := range dst.PortRows {
			found = found || old == v
		}
		if !found {
			dst.PortRows = append(dst.PortRows, v)
		}
	}
}

// Inspect every actual platform row and kernel veth. A missing API owner may
// use only its positive native journal; an orphan/unattributable row is Unknown.
func (o *NativeRuntimeObserver) captureScopeAttachments(ctx context.Context, id *lab.OwnedRuntimeIdentity, owners []lab.OwnedRuntimeIdentity, absent ...bool) error {
	final := len(absent) > 0 && absent[0]
	ovs := o.Network.OVS
	ovs.mu.Lock()
	rows, err := ovs.portOwnersLocked()
	ovs.mu.Unlock()
	if err != nil {
		return err
	}
	// Fabric rows are independent of Pod rows. Inspect the actual database even
	// when the controller declared a zero-leg scope or its API owner vanished.
	var connections lab.ConnectionList
	var allFabric []lab.OwnedFabricPort
	if err := o.Reader.List(ctx, &connections); err != nil {
		return err
	}
	ops := []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: "Port", Where: []ovsdb.Condition{}, Columns: []string{"_uuid", "name", "external_ids"}}}
	ovs.mu.Lock()
	result, err := ovs.client.Transact(ovs.ctx, ops...)
	ovs.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := ovsdb.CheckOperationResults(result, ops); err != nil {
		return err
	}
	for _, actual := range result[0].Rows {
		name, _ := actual["name"].(string)
		ids, _ := actual["external_ids"].(ovsdb.OvsMap)
		uid, _ := ids.GoMap[fabricOwnerExternalID].(string)
		if uid == "" && !strings.HasPrefix(name, "pt") {
			continue
		}
		uuid, ok := actual["_uuid"].(ovsdb.UUID)
		if !ok || uid == "" {
			return fmt.Errorf("unattributed native fabric row")
		}
		known, owned := false, false
		for _, conn := range connections.Items {
			if string(conn.UID) != uid {
				continue
			}
			ref, ok := nativeOwnerReference(conn.OwnerReferences, "Lab")
			if !ok {
				return ErrPortOwnerUnknown
			}
			var parent lab.Lab
			if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: conn.Namespace, Name: conn.Spec.LabRef}, &parent); err != nil {
				return err
			}
			if ref.UID != parent.UID {
				return ErrPortOwnerChanged
			}
			known = true
			owned = conn.Namespace == id.Namespace && string(parent.UID) == id.OwnerUID && id.ScopeKind == "LabFabric"
		}
		entry := lab.OwnedFabricPort{Key: name, OwnerUID: uid, RowUUID: uuid.GoUUID}
		for _, prior := range id.FabricPorts {
			if prior.Key == name {
				if prior != entry {
					return ErrPortOwnerChanged
				}
				known, owned = true, true
			}
		}
		if !known {
			return fmt.Errorf("unattributed native fabric owner")
		}
		allFabric = append(allFabric, entry)
		if owned {
			if final {
				return fmt.Errorf("owned native fabric row remains after retirement")
			}
			found := false
			for _, prior := range id.FabricPorts {
				found = found || prior == entry
			}
			if !found {
				id.FabricPorts = append(id.FabricPorts, entry)
			}
		}
	}
	for key, uid := range rows {
		known := false
		owned := false
		for _, owner := range owners {
			if owner.PodUID == string(uid) {
				known = true
				owned = owned || scopeOwnsPod(*id, owner)
			}
		}
		if !known {
			return fmt.Errorf("unattributed native platform attachment")
		}
		if !owned {
			continue
		}
		if final {
			return fmt.Errorf("owned native attachment remains after retirement")
		}
		ovs.mu.Lock()
		row, err := ovs.portSnapshotLocked(key)
		ovs.mu.Unlock()
		if err != nil {
			return err
		}
		if row == nil {
			return ErrPortOwnerChanged
		}
		if !runtimeContainsID(id.PortKeys, key) {
			id.PortKeys = append(id.PortKeys, key)
		}
		entry := lab.OwnedFabricPort{Key: key, OwnerUID: string(uid), RowUUID: row.UUID}
		found := false
		for _, old := range id.PortRows {
			found = found || old == entry
		}
		if !found {
			id.PortRows = append(id.PortRows, entry)
		}
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		key := link.Attrs().Name
		if !ValidPortKey(key) {
			continue
		}
		if _, present := rows[key]; present {
			continue
		}
		known := false
		owned := runtimeContainsID(id.PortKeys, key)
		for _, owner := range owners {
			known = known || runtimeContainsID(owner.PortKeys, key)
			owned = owned || scopeOwnsPod(*id, owner) && runtimeContainsID(owner.PortKeys, key)
		}
		if !known {
			return fmt.Errorf("unattributed native platform kernel link")
		}
		if owned {
			return fmt.Errorf("owned native kernel link remains without its OVS owner row")
		}
	}
	flows, err := o.Network.Flows.readNativeFlows(ctx)
	if err != nil {
		return err
	}
	// Every scope-owned flow must have an actual port or positive VNI lease. A
	// detached flow with no attributable port cannot become an empty certificate.
	if err := o.Network.Flows.client.RefreshPorts(); err != nil {
		return err
	}
	ports := o.Network.Flows.client.Ports()
	mapped := map[uint32]bool{}
	for _, no := range ports {
		mapped[no] = true
	}
	knownDomains := map[uint64]bool{}
	var devices lab.DeviceList
	if err := o.Reader.List(ctx, &devices); err != nil {
		return err
	}
	addLease := func(uid string, index *uint, lease *lab.VNILease) error {
		if index == nil || lease == nil {
			return nil
		}
		if err := poolpkg.ValidateLease(ctx, o.Reader, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: *index, PoolUID: lease.PoolUID, OwnerUID: uid, Generation: lease.Generation}); err != nil {
			return err
		}
		knownDomains[uint64(*index)] = true
		return nil
	}
	for _, d := range devices.Items {
		if err := addLease(string(d.UID), d.Status.VNI, d.Status.VNILease); err != nil {
			return err
		}
	}
	for _, c := range connections.Items {
		if err := addLease(string(c.UID), c.Status.VNI, c.Status.VNILease); err != nil {
			return err
		}
	}
	for _, flow := range flows {
		if flow.InPort != 0 && !mapped[flow.InPort] {
			return fmt.Errorf("unattributed native orphan flow")
		}
		if flow.InPort != 0 && !attributableFlowPort(flow.InPort, ports, rows, allFabric) && ports[GenevePort] != flow.InPort {
			return fmt.Errorf("native ingress port has no exact owner")
		}
		if flow.VNI != 0 && !knownDomains[flow.VNI] {
			return fmt.Errorf("unattributed native VNI flow without a current lease")
		}
		if flow.InPort == 0 && flow.VNI == 0 && !strings.HasSuffix(strings.TrimSpace(flow.Raw), "actions=drop") {
			return fmt.Errorf("native flow has no attributable ingress or VNI domain")
		}
		for _, output := range nativeOutput.FindAllStringSubmatch(flow.Raw, -1) {
			no, err := strconv.ParseUint(output[1], 10, 32)
			if err != nil || !mapped[uint32(no)] {
				return fmt.Errorf("unattributed native orphan output flow")
			}
		}
		if final {
			for _, binding := range id.VNIBindings {
				if flow.VNI == uint64(binding.VNI) {
					return fmt.Errorf("owned native VNI flow remains after retirement")
				}
			}
		}
	}
	return nil
}

var nativePodCgroup = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})(?:\.slice|/|$)`)

// Namespace attribution comes from positive native metadata/API/journal UIDs.
// A populated orphan pod cgroup cannot disappear with its container metadata.
func (o *NativeRuntimeObserver) captureScopeCgroups(id *lab.OwnedRuntimeIdentity, known map[string]string, owners []lab.OwnedRuntimeIdentity) error {
	return filepath.WalkDir(o.CgroupRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		match := nativePodCgroup.FindStringSubmatch(path)
		if len(match) == 0 {
			return nil
		}
		uid := strings.ReplaceAll(match[1], "_", "-")
		namespace, attributed := known[uid]
		owned := false
		for _, owner := range owners {
			if owner.PodUID == uid {
				attributed = true
				namespace = owner.Namespace
				owned = owned || scopeOwnsPod(*id, owner)
			}
		}
		if id.ScopeKind == "GroupScope" && namespace == id.Namespace {
			owned = true
		}
		populated, err := o.cgroupsPresent([]string{path})
		if err != nil {
			return err
		}
		if !attributed && populated {
			return fmt.Errorf("populated native cgroup has no positive owner inventory")
		}
		if owned && !runtimeContainsID(id.CgroupPaths, path) {
			id.CgroupPaths = append(id.CgroupPaths, path)
		}
		return nil
	})
}

func attributableFlowPort(number uint32, ports map[string]uint32, rows map[string]types.UID, fabric []lab.OwnedFabricPort) bool {
	for name, no := range ports {
		if no != number {
			continue
		}
		if rows[name] != "" {
			return true
		}
		for _, row := range fabric {
			if row.Key == name && row.OwnerUID != "" && row.RowUUID != "" {
				return true
			}
		}
	}
	return false
}
