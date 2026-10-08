//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	ovsclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sync"
	"testing"
)

// This adapter applies the actual OVS transaction wait predicates atomically.
// It is a native API fixture, not qualification of an installed OVS switch.
type residualFabricDB struct {
	ovsclient.Client
	mu              sync.Mutex
	ports           map[string]ovsdb.Row
	interfaces      map[string]ovsdb.Row
	claims, deletes int
	beforeClaim     func()
}

func residualCopyRow(r ovsdb.Row) ovsdb.Row {
	out := ovsdb.Row{}
	for k, v := range r {
		out[k] = v
	}
	return out
}
func residualMatches(row ovsdb.Row, conditions []ovsdb.Condition) bool {
	for _, c := range conditions {
		if !reflect.DeepEqual(row[c.Column], c.Value) {
			return false
		}
	}
	return true
}
func (d *residualFabricDB) Transact(_ context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, op := range ops {
		if op.Op == ovsdb.OperationUpdate && op.Table == "Port" && d.beforeClaim != nil {
			hook := d.beforeClaim
			d.beforeClaim = nil
			hook()
			break
		}
	}
	result := make([]ovsdb.OperationResult, len(ops))
	// Validate all CAS waits before applying any mutation.
	for n, op := range ops {
		if op.Op != ovsdb.OperationWait {
			continue
		}
		table := d.ports
		if op.Table == "Interface" {
			table = d.interfaces
		}
		var rows []ovsdb.Row
		for _, row := range table {
			if residualMatches(row, op.Where) {
				projected := ovsdb.Row{}
				for _, col := range op.Columns {
					projected[col] = row[col]
				}
				rows = append(rows, projected)
			}
		}
		if len(rows) != len(op.Rows) || len(rows) != 1 || !reflect.DeepEqual(rows[0], op.Rows[0]) {
			result[n].Error = "timed out"
			return result, nil
		}
	}
	for n, op := range ops {
		table := d.ports
		if op.Table == "Interface" {
			table = d.interfaces
		}
		switch op.Op {
		case ovsdb.OperationSelect:
			for _, row := range table {
				if residualMatches(row, op.Where) {
					result[n].Rows = append(result[n].Rows, residualCopyRow(row))
				}
			}
		case ovsdb.OperationInsert:
			row := residualCopyRow(op.Row)
			name := row["name"].(string)
			row["_uuid"] = ovsdb.UUID{GoUUID: op.Table + "-" + name}
			table[name] = row
		case ovsdb.OperationUpdate:
			for _, row := range table {
				if residualMatches(row, op.Where) {
					for k, v := range op.Row {
						row[k] = v
					}
					result[n].Count++
					if op.Table == "Port" {
						d.claims++
					}
				}
			}
		case ovsdb.OperationDelete:
			for name, row := range table {
				if residualMatches(row, op.Where) {
					delete(table, name)
					d.deletes++
					result[n].Count++
				}
			}
		}
	}
	return result, nil
}
func (d *residualFabricDB) List(_ context.Context, target interface{}) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch out := target.(type) {
	case *[]OVSBridge:
		*out = []OVSBridge{{UUID: "bridge", Name: "br-ovs"}}
	case *[]OVSPort:
		*out = nil
		for _, row := range d.ports {
			ids := map[string]string{}
			for k, v := range row["external_ids"].(ovsdb.OvsMap).GoMap {
				ids[k.(string)] = v.(string)
			}
			*out = append(*out, OVSPort{UUID: row["_uuid"].(ovsdb.UUID).GoUUID, Name: row["name"].(string), ExternalIDs: ids})
		}
	default:
		return fmt.Errorf("unsupported adapter list %T", target)
	}
	return nil
}
func (d *residualFabricDB) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	for _, item := range models {
		switch v := item.(type) {
		case *OVSPort:
			ids, _ := ovsdb.NewOvsMap(v.ExternalIDs)
			ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationInsert, Table: "Port", Row: ovsdb.Row{"name": v.Name, "external_ids": ids}})
		case *OVSInterface:
			options, _ := ovsdb.NewOvsMap(v.Options)
			ops = append(ops, ovsdb.Operation{Op: ovsdb.OperationInsert, Table: "Interface", Row: ovsdb.Row{"name": v.Name, "type": v.Type, "options": options}})
		default:
			return nil, fmt.Errorf("unsupported adapter create %T", item)
		}
	}
	return ops, nil
}
func (d *residualFabricDB) Where(models ...model.Model) ovsclient.ConditionalAPI {
	return residualFabricConditional{item: models[0]}
}

type residualFabricConditional struct {
	ovsclient.ConditionalAPI
	item model.Model
}

func (r residualFabricConditional) Mutate(model.Model, ...model.Mutation) ([]ovsdb.Operation, error) {
	return nil, nil
}
func (r residualFabricConditional) Delete() ([]ovsdb.Operation, error) {
	p := r.item.(*OVSPort)
	return []ovsdb.Operation{{Op: ovsdb.OperationDelete, Table: "Port", Where: []ovsdb.Condition{ovsdb.NewCondition("_uuid", ovsdb.ConditionEqual, ovsdb.UUID{GoUUID: p.UUID})}}}, nil
}
func residualFabricFixture(t *testing.T, legacy bool) (*ConnectionReconciler, *lab.Connection, lab.Device, lab.Device, *residualFabricDB) {
	t.Helper()
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = lab.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab"}}
	refs := []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: parent.UID}}
	conn := &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "conn", Namespace: "ns", UID: "connection", OwnerReferences: refs}, Spec: lab.ConnectionSpec{LabRef: "l", Endpoints: []lab.EndpointSpec{{Device: "a"}, {Device: "b"}}}}
	a := lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "device-a", Namespace: "ns", UID: "a", OwnerReferences: refs}, Spec: lab.DeviceSpec{Name: "a", LabRef: "l", Type: lab.DeviceTypeUnmanagedSwitch}}
	b := *a.DeepCopy()
	b.Name = "device-b"
	b.UID = "b"
	b.Spec.Name = "b"
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool", Labels: map[string]string{poolpkg.PoolGroupLabel: "vni", poolpkg.PoolStateLabel: poolpkg.PoolStateEmpty, poolpkg.LatestPoolLabel: "true"}}, Spec: allocation.PoolSpec{Size: names.VNIPoolSize}, Status: allocation.PoolStatus{BitMap: bitmap, Free: free}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(pool, &lab.Device{}, conn).WithObjects(parent, conn, &a, &b, pool).Build()
	for _, d := range []*lab.Device{&a, &b} {
		lease, err := poolpkg.AllocateOwnedIndex(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, string(d.UID))
		if err != nil {
			t.Fatal(err)
		}
		d.Status.VNI = &lease.Index
		d.Status.VNILease = &lab.VNILease{PoolUID: lease.PoolUID, Generation: lease.Generation}
		if err := c.Status().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
		_ = c.Get(ctx, client.ObjectKeyFromObject(d), d)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(conn), conn)
	db := &residualFabricDB{ports: map[string]ovsdb.Row{}, interfaces: map[string]ovsdb.Row{}}
	keys := []string{patchPortName("ns", "conn", "a"), patchPortName("ns", "conn", "b")}
	if legacy {
		keys = []string{legacyPatchPortName("conn", "a"), legacyPatchPortName("conn", "b")}
		for i, key := range keys {
			ids, _ := ovsdb.NewOvsMap(map[string]string{})
			options, _ := ovsdb.NewOvsMap(map[string]string{"peer": keys[1-i]})
			db.ports[key] = ovsdb.Row{"_uuid": ovsdb.UUID{GoUUID: "Port-" + key}, "name": key, "external_ids": ids}
			db.interfaces[key] = ovsdb.Row{"_uuid": ovsdb.UUID{GoUUID: "Interface-" + key}, "name": key, "type": "patch", "options": options}
		}
	}
	flows := &FlowManager{client: scopeFlowAdapterWith(t, func() map[string]uint32 { return map[string]uint32{keys[0]: 7, keys[1]: 8} }, nil), nativeFlowRead: func(context.Context) ([]nativeFlow, error) {
		return []nativeFlow{{InPort: 7, VNI: uint64(*a.Status.VNI), Raw: "actions=drop"}, {InPort: 8, VNI: uint64(*b.Status.VNI), Raw: "actions=drop"}}, nil
	}}
	return &ConnectionReconciler{Client: c, Reader: c, OVS: &OVSManager{ctx: ctx, bridge: "br-ovs", client: db}, Flows: flows}, conn, a, b, db
}
func TestResidualDefaultOffOwnedPatchPairCreateDelete(t *testing.T) {
	r, c, _, _, db := residualFabricFixture(t, false)
	keys := []string{patchPortName("ns", "conn", "a"), patchPortName("ns", "conn", "b")}
	if err := r.OVS.AddPatchPairOwned(keys[0], keys[1], c.UID); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if err := r.OVS.DelFabricPortOwned(key, c.UID, r.Flows); err != nil {
			t.Fatalf("default-off physical cleanup blocked: %v", err)
		}
	}
	if len(db.ports) != 0 || db.deletes != 2 || db.claims != 0 {
		t.Fatalf("pair not physically removed: %+v", db)
	}
	if r.OVS.FabricPrepare != nil || r.OVS.FabricRetirement != nil {
		t.Fatal("fixture fabricated lifecycle callbacks")
	}
}
func TestResidualLegacyPatchPairRequiresExactTopologyLeasePeersAndFlowDomain(t *testing.T) {
	for _, scenario := range []string{"success", "foreign-domain", "foreign-peer", "foreign-owner", "missing-lease", "pool-replaced", "connection-replaced", "endpoint-replaced", "ambiguous-name", "peer-replaced-at-claim", "row-replaced-at-barrier"} {
		t.Run(scenario, func(t *testing.T) {
			r, c, a, b, db := residualFabricFixture(t, true)
			ctx := context.Background()
			key := legacyPatchPortName(c.Name, a.Spec.Name)
			switch scenario {
			case "foreign-domain":
				r.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) { return []nativeFlow{{InPort: 7, VNI: 999}}, nil }
			case "foreign-peer":
				options, _ := ovsdb.NewOvsMap(map[string]string{"peer": "foreign"})
				db.interfaces[key]["options"] = options
			case "foreign-owner":
				ids, _ := ovsdb.NewOvsMap(map[string]string{fabricOwnerExternalID: "foreign"})
				db.ports[key]["external_ids"] = ids
			case "missing-lease":
				a.Status.VNILease = nil
			case "pool-replaced":
				var p allocation.Pool
				_ = r.Get(ctx, client.ObjectKey{Namespace: names.SystemNamespace, Name: "vni-0"}, &p)
				p.UID = "replacement"
				_ = r.Update(ctx, &p)
			case "connection-replaced":
				newer := c.DeepCopy()
				newer.UID = "replacement"
				_ = r.Update(ctx, newer)
			case "endpoint-replaced":
				newer := a.DeepCopy()
				newer.UID = "replacement"
				_ = r.Update(ctx, newer)
			case "ambiguous-name":
				other := c.DeepCopy()
				other.Namespace = "other"
				other.UID = "other"
				other.ResourceVersion = ""
				_ = r.Create(ctx, other)
			case "peer-replaced-at-claim":
				db.beforeClaim = func() { db.interfaces[key]["_uuid"] = ovsdb.UUID{GoUUID: "replacement"} }
			case "row-replaced-at-barrier":
				r.Flows.client = scopeFlowAdapterWith(t, func() map[string]uint32 {
					return map[string]uint32{key: 7, legacyPatchPortName(c.Name, b.Spec.Name): 8}
				}, func() { db.mu.Lock(); defer db.mu.Unlock(); db.ports[key]["_uuid"] = ovsdb.UUID{GoUUID: "replacement"} })
			}
			err := r.migrateLegacySwitchPair(ctx, c, a, b)
			if scenario == "success" {
				if err != nil || len(db.ports) != 0 || db.claims != 2 || db.deletes != 2 {
					t.Fatalf("exact legacy physical upgrade failed: %v ports=%d claims=%d deletes=%d", err, len(db.ports), db.claims, db.deletes)
				}
			} else if err == nil || db.deletes != 0 {
				t.Fatalf("unsafe migration accepted %s: err=%v deletes=%d", scenario, err, db.deletes)
			}
			if r.OVS.FabricPrepare != nil || r.OVS.FabricRetirement != nil {
				t.Fatal("legacy migration supplied lifecycle credit")
			}
		})
	}
}
