//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strconv"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	"github.com/ovn-org/libovsdb/ovsdb"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var nativeOutput = regexp.MustCompile(`output:([0-9]+)`)

// This authority upgrades only the old physical pair. It never creates a birth
// receipt or lifecycle release witness; all lifecycle callbacks are excluded.
func (r *ConnectionReconciler) migrateLegacySwitchPair(ctx context.Context, conn *lab.Connection, a, b lab.Device) error {
	keys := []string{legacyPatchPortName(conn.Name, a.Spec.Name), legacyPatchPortName(conn.Name, b.Spec.Name)}
	r.OVS.vethMu.Lock()
	defer r.OVS.vethMu.Unlock()
	r.OVS.mu.Lock()
	defer r.OVS.mu.Unlock()
	first, err := r.OVS.fabricSnapshotLocked(keys[0])
	if err != nil {
		return err
	}
	second, err := r.OVS.fabricSnapshotLocked(keys[1])
	if err != nil {
		return err
	}
	if first == nil && second == nil {
		return nil
	}
	if first == nil || second == nil {
		return fmt.Errorf("legacy pair incomplete")
	}
	validate := func() error { return r.validateLegacyTopology(ctx, conn, a, b) }
	if err := validate(); err != nil {
		return err
	}
	peerA, err := r.OVS.patchPeerSnapshotLocked(first, keys[1])
	if err != nil {
		return err
	}
	peerB, err := r.OVS.patchPeerSnapshotLocked(second, keys[0])
	if err != nil {
		return err
	}
	if err := r.Flows.client.RefreshPorts(); err != nil {
		return err
	}
	ports := r.Flows.client.Ports()
	flows, err := r.Flows.readNativeFlows(ctx)
	if err != nil {
		return err
	}
	if err := legacyFlowDomainsProven(keys, a, b, ports, flows); err != nil {
		return err
	}
	// Claim both rows and exact Interface peer snapshots in one OVS transaction.
	if err := validate(); err != nil {
		return err
	}
	if err := r.OVS.claimLegacyPairLocked(first, second, peerA, peerB, string(conn.UID)); err != nil {
		return err
	}
	for _, row := range []*OVSPort{first, second} {
		row.ExternalIDs[fabricOwnerExternalID] = string(conn.UID)
		row.ExternalIDs["cice-physical-migration"] = "true"
		if err := r.Flows.retirePort(row.Name); err != nil {
			return err
		}
	}
	// Revalidate the API owners, leases and both native rows after the barriers.
	if err := validate(); err != nil {
		return err
	}
	for i, row := range []*OVSPort{first, second} {
		current, err := r.OVS.fabricSnapshotLocked(row.Name)
		if err != nil {
			return err
		}
		if current == nil || current.UUID != row.UUID || current.ExternalIDs[fabricOwnerExternalID] != string(conn.UID) {
			return ErrPortOwnerChanged
		}
		peer, err := r.OVS.patchPeerSnapshotLocked(row, keys[1-i])
		if err != nil {
			return err
		}
		original := peerA
		if i == 1 {
			original = peerB
		}
		if !reflect.DeepEqual(peer, original) {
			return ErrPortOwnerChanged
		}
	}
	for _, row := range []*OVSPort{first, second} {
		if err := validate(); err != nil {
			return err
		}
		if err := r.OVS.delPortLocked(row); err != nil {
			return err
		}
	}
	return nil
}

// Every owner comes from the current unique topology and exact reserved lease.
func (r *ConnectionReconciler) validateLegacyTopology(ctx context.Context, conn *lab.Connection, a, b lab.Device) error {
	var current lab.Connection
	if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(conn), &current); err != nil {
		return err
	}
	if current.UID != conn.UID || current.ResourceVersion != conn.ResourceVersion || !reflect.DeepEqual(current.Spec, conn.Spec) {
		return ErrPortOwnerChanged
	}
	if len(current.Spec.Endpoints) != 2 || a.UID == b.UID || current.Spec.Endpoints[0].Device != a.Spec.Name || current.Spec.Endpoints[1].Device != b.Spec.Name {
		return ErrPortOwnerChanged
	}
	var all lab.ConnectionList
	if err := r.directReader().List(ctx, &all); err != nil {
		return err
	}
	matches := 0
	for _, candidate := range all.Items {
		if candidate.Name == conn.Name {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("legacy global pair has ambiguous Connection topology")
	}
	var parent lab.Lab
	if err := r.directReader().Get(ctx, client.ObjectKey{Namespace: conn.Namespace, Name: conn.Spec.LabRef}, &parent); err != nil {
		return err
	}
	ref, ok := nativeOwnerReference(current.OwnerReferences, ownerKindLab)
	if !ok || ref.UID != parent.UID {
		return ErrPortOwnerChanged
	}
	for _, expected := range []lab.Device{a, b} {
		var d lab.Device
		if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(&expected), &d); err != nil {
			return err
		}
		if d.UID != expected.UID || d.ResourceVersion != expected.ResourceVersion || !reflect.DeepEqual(d.Spec, expected.Spec) || !reflect.DeepEqual(d.Status.VNI, expected.Status.VNI) || !reflect.DeepEqual(d.Status.VNILease, expected.Status.VNILease) || d.Spec.LabRef != conn.Spec.LabRef {
			return ErrPortOwnerChanged
		}
		ref, ok := nativeOwnerReference(d.OwnerReferences, ownerKindLab)
		if !ok || ref.UID != parent.UID {
			return ErrPortOwnerChanged
		}
		if d.Status.VNI == nil || d.Status.VNILease == nil {
			return fmt.Errorf("legacy pair endpoint lease unavailable")
		}
		if err := poolpkg.ValidateLease(ctx, r.directReader(), names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: *d.Status.VNI, PoolUID: d.Status.VNILease.PoolUID, OwnerUID: string(d.UID), Generation: d.Status.VNILease.Generation}); err != nil {
			return err
		}
	}
	return nil
}
func (m *OVSManager) patchPeerSnapshotLocked(row *OVSPort, peer string) (ovsdb.Row, error) {
	ops := []ovsdb.Operation{{Op: ovsdb.OperationSelect, Table: "Interface", Where: []ovsdb.Condition{ovsdb.NewCondition(cniNameKey, ovsdb.ConditionEqual, row.Name)}, Columns: []string{ovsUUIDColumn, cniNameKey, cniTypeKey, "options"}}}
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return nil, err
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return nil, err
	}
	if len(results[0].Rows) != 1 || results[0].Rows[0][cniTypeKey] != "patch" {
		return nil, ErrPortOwnerUnknown
	}
	found := results[0].Rows[0]
	options, ok := found["options"].(ovsdb.OvsMap)
	if !ok || options.GoMap["peer"] != peer {
		return nil, ErrPortOwnerChanged
	}
	if uuid, ok := found[ovsUUIDColumn].(ovsdb.UUID); !ok || uuid.GoUUID == "" {
		return nil, ErrPortOwnerUnknown
	}
	return found, nil
}
func (m *OVSManager) claimLegacyPairLocked(a, b *OVSPort, peerA, peerB ovsdb.Row, uid string) error {
	var ops []ovsdb.Operation
	for _, peer := range []ovsdb.Row{peerA, peerB} {
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationWait, Table: "Interface", Where: []ovsdb.Condition{ovsdb.NewCondition(ovsUUIDColumn, ovsdb.ConditionEqual, peer[ovsUUIDColumn])}, Columns: []string{ovsUUIDColumn, cniNameKey, cniTypeKey, "options"}, Rows: []ovsdb.Row{peer}, Until: "==", Timeout: ptrZeroTimeout()})
	}
	for _, row := range []*OVSPort{a, b} {
		if owner := row.ExternalIDs[fabricOwnerExternalID]; owner != "" && owner != uid {
			return ErrPortOwnerChanged
		}
		old, _ := ovsdb.NewOvsMap(row.ExternalIDs)
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationWait, Table: ovsPortTable, Where: []ovsdb.Condition{ovsdb.NewCondition(ovsUUIDColumn, ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: row.UUID})}, Columns: []string{nativeExternalIDsColumn, cniNameKey}, Until: "==", Rows: []ovsdb.Row{{nativeExternalIDsColumn: old, cniNameKey: row.Name}}, Timeout: ptrZeroTimeout()})
		ids := map[string]string{}
		for key, value := range row.ExternalIDs {
			ids[key] = value
		}
		ids[fabricOwnerExternalID] = uid
		ids["cice-physical-migration"] = "true"
		updated, _ := ovsdb.NewOvsMap(ids)
		ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationUpdate, Table: ovsPortTable, Where: []ovsdb.Condition{ovsdb.NewCondition(ovsUUIDColumn, ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: row.UUID})}, Row: ovsdb.Row{nativeExternalIDsColumn: updated}})
	}
	results, err := m.client.Transact(m.ctx, ops...)
	if err != nil {
		return err
	}
	_, err = ovsdb.CheckOperationResults(results, ops)
	return err
}
func ptrZeroTimeout() *int { v := 0; return &v }

func legacyFlowDomainsProven(keys []string, a, b lab.Device, ports map[string]uint32, flows []nativeFlow) error {
	for i, key := range keys {
		no, ok := ports[key]
		if !ok {
			return fmt.Errorf("legacy pair native ofport unavailable")
		}
		expected := uint64(*a.Status.VNI)
		if i == 1 {
			expected = uint64(*b.Status.VNI)
		}
		matched := false
		for _, flow := range flows {
			if flow.InPort == no {
				if !flow.HasVNI || flow.VNI != expected {
					return fmt.Errorf("legacy ingress is in a foreign VNI domain")
				}
				matched = true
			}
			for _, out := range nativeOutput.FindAllStringSubmatch(flow.Raw, -1) {
				number, _ := strconv.ParseUint(out[1], 10, 32)
				if uint32(number) == no && (!flow.HasVNI || flow.VNI != expected) {
					return fmt.Errorf("legacy output belongs to a foreign VNI domain")
				}
			}
		}
		if !matched {
			return fmt.Errorf("legacy native flow domain unproved")
		}
	}
	return nil
}
