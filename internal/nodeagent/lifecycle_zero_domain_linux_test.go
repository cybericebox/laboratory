//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"testing"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResidualZeroDomainParserRejectsContradictoryMetadata(t *testing.T) {
	if _, err := parseNativeFlows("table=6,metadata=0 actions=load:0x5->metadata,drop"); err == nil {
		t.Fatal("explicit zero followed by a foreign domain was accepted")
	}
}
func TestResidualZeroBindingDefaultDropDoesNotBecomeOwnedFlowDebt(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	id.VNIBindings = []lab.OwnedVNI{{UID: "zero-owner", VNI: 0}}
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return parseNativeFlows("table=0,priority=0 actions=drop")
	}
	if err := o.captureScopeAttachments(context.Background(), &id, nil, true); err != nil {
		t.Fatal("metadata-free default drop became VNI-zero debt:", err)
	}
}
func TestResidualExplicitZeroWithoutCurrentLeaseRemainsUnknown(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return parseNativeFlows("table=6,metadata=0 actions=drop")
	}
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Unknown" || report.AttachmentsAbsentAt != nil {
		t.Fatalf("orphan explicit zero was treated as default drop: %+v", report)
	}
}
func residualActualZeroLease(t *testing.T, legacy bool) (*ConnectionReconciler, *lab.Connection, lab.Device, lab.Device, *residualFabricDB, lab.OwnedVNI) {
	t.Helper()
	ctx := context.Background()
	r, conn, a, b, db := residualFabricFixture(t, legacy)
	original := poolpkg.Lease{Index: *a.Status.VNI, PoolUID: a.Status.VNILease.PoolUID, OwnerUID: string(a.UID), Generation: a.Status.VNILease.Generation}
	if err := poolpkg.ReleaseOwnedIndex(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, original); err != nil {
		t.Fatal(err)
	}
	lease, err := poolpkg.PinExistingIndex(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, 0, string(a.UID))
	if err != nil {
		t.Fatal(err)
	}
	a.Status.VNI = &lease.Index
	a.Status.VNILease = &lab.VNILease{PoolUID: lease.PoolUID, Generation: lease.Generation}
	if err := r.Status().Update(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(&a), &a); err != nil {
		t.Fatal(err)
	}
	var parent lab.Lab
	if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "l"}, &parent); err != nil {
		t.Fatal(err)
	}
	parent.Generation = 3
	parent.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "zero-stop", Revision: 1, SnapshotMode: "Skip"}
	if err := r.Update(ctx, &parent); err != nil {
		t.Fatal(err)
	}
	binding := lab.OwnedVNI{Kind: "Device", Namespace: a.Namespace, Name: a.Name, UID: string(a.UID), VNI: 0, PoolUID: lease.PoolUID, LeaseGeneration: lease.Generation, OwnerUID: string(parent.UID), OperationID: "zero-stop", Revision: 1, Generation: 3}
	return r, conn, a, b, db, binding
}
func TestResidualActualZeroLeaseRetiresThroughObserverAndOVSBarrier(t *testing.T) {
	for _, entry := range []string{"observer", "ovs"} {
		t.Run(entry, func(t *testing.T) {
			r, _, _, _, _, binding := residualActualZeroLease(t, false)
			barriers := 0
			r.Flows.client = scopeFlowAdapterWith(t, func() map[string]uint32 { return nil }, func() { barriers++ })
			o := &NativeRuntimeObserver{Reader: r.Reader, NodeName: "node", BootID: "boot", JournalDir: t.TempDir()}
			var err error
			if entry == "observer" {
				r.OVS.vethMu.Lock()
				err = o.retireVNI(context.Background(), binding, r.Flows)
				r.OVS.vethMu.Unlock()
			} else {
				r.OVS.VNIRetirement = o.retireVNI
				err = r.OVS.RetireVNIOwned(context.Background(), binding, r.Flows)
			}
			if err != nil || barriers != 1 || !o.vniReleased(binding) {
				t.Fatalf("actual Pool-owned zero could not retire: %v barriers=%d released=%v", err, barriers, o.vniReleased(binding))
			}
		})
	}
}
func TestResidualLegacyZeroIngressStillRequiresExplicitDomain(t *testing.T) {
	r, conn, a, b, db, _ := residualActualZeroLease(t, true)
	r.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return parseNativeFlows(fmt.Sprintf("table=0,in_port=7 actions=drop\n table=0,in_port=8 actions=load:0x%x->metadata,drop", *b.Status.VNI))
	}
	err := r.migrateLegacySwitchPair(context.Background(), conn, a, b)
	if err == nil || db.deletes != 0 {
		t.Fatalf("missing metadata impersonated valid zero lease: %v deletes=%d", err, db.deletes)
	}
}
func TestResidualZeroPresenceFlagSeparatesExplicitMetadataFromDefaultDrop(t *testing.T) {
	flows, err := parseNativeFlows("table=6,metadata=0 actions=drop\n table=0,priority=0 actions=drop\n table=0,in_port=7 actions=load:0->OXM_OF_METADATA[],drop")
	if err != nil || len(flows) != 3 || !flows[0].HasVNI || flows[1].HasVNI || !flows[2].HasVNI || flows[0].VNI != 0 || flows[2].VNI != 0 {
		t.Fatalf("explicit zero/absent metadata not distinguished: %+v %v", flows, err)
	}
}
func TestResidualZeroScopeStillHoldsActualOwnedDomainAfterBarrier(t *testing.T) {
	r, _, _, _, _, binding := residualActualZeroLease(t, false)
	o, id := scopeProducerAdapter(t, nil, nil)
	o.Reader = r.Reader
	id.VNIBindings = []lab.OwnedVNI{binding}
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return parseNativeFlows("table=6,metadata=0 actions=drop")
	}
	if err := o.captureScopeAttachments(context.Background(), &id, nil, true); err == nil {
		t.Fatal("actual leased zero domain survived final barrier but was credited absent")
	}
}
func TestResidualDefaultOffZeroRetirementRequiresActualReaderAuthorityAcrossBarrier(t *testing.T) {
	for _, scenario := range []string{"success", "constructed-tuple-no-authority", "missing-pool", "missing-owner", "missing-generation", "wrong-pool", "wrong-generation", "wrong-owner", "wrong-kind", "missing-api-owner", "foreign-api-owner", "changed-vni", "pool-replaced-at-barrier", "owner-replaced-at-barrier"} {
		t.Run(scenario, func(t *testing.T) {
			r, _, a, _, _, binding := residualActualZeroLease(t, false)
			ctx := context.Background()
			barriers := 0
			r.Flows.client = scopeFlowAdapterWith(t, func() map[string]uint32 { return nil }, func() {
				barriers++
				if scenario == "pool-replaced-at-barrier" {
					var p allocation.Pool
					if err := r.Get(ctx, client.ObjectKey{Namespace: names.SystemNamespace, Name: "vni-0"}, &p); err != nil {
						t.Error(err)
						return
					}
					p.UID = "replacement"
					if err := r.Update(ctx, &p); err != nil {
						t.Error(err)
					}
				}
				if scenario == "owner-replaced-at-barrier" {
					var d lab.Device
					if err := r.Get(ctx, client.ObjectKeyFromObject(&a), &d); err != nil {
						t.Error(err)
						return
					}
					d.UID = "replacement"
					if err := r.Update(ctx, &d); err != nil {
						t.Error(err)
					}
				}
			})
			switch scenario {
			case "missing-pool":
				binding.PoolUID = ""
			case "missing-owner":
				binding.OwnerUID = ""
			case "missing-generation":
				binding.LeaseGeneration = 0
			case "wrong-pool":
				binding.PoolUID = "foreign"
			case "wrong-generation":
				binding.LeaseGeneration++
			case "wrong-owner":
				binding.OwnerUID = "foreign"
			case "wrong-kind":
				binding.Kind = "foreign"
			case "missing-api-owner":
				if err := r.Delete(ctx, &a); err != nil {
					t.Fatal(err)
				}
			case "foreign-api-owner":
				a.OwnerReferences[0].UID = "foreign"
				if err := r.Update(ctx, &a); err != nil {
					t.Fatal(err)
				}
			case "changed-vni":
				index := uint(9)
				a.Status.VNI = &index
				if err := r.Status().Update(ctx, &a); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if scenario == "constructed-tuple-no-authority" {
				err = r.OVS.RetireVNIOwned(ctx, binding, r.Flows)
			} else {
				err = r.OVS.RetireVNIOwned(ctx, binding, r.Flows, r.zeroVNIRetirementAuthority())
			}
			if scenario == "success" {
				if err != nil || barriers != 1 {
					t.Fatalf("valid zero physical fallback failed: %v barriers=%d", err, barriers)
				}
			} else {
				if err == nil {
					t.Fatal("unproved/foreign zero lease accepted")
				}
				if scenario != "pool-replaced-at-barrier" && scenario != "owner-replaced-at-barrier" && barriers != 0 {
					t.Fatal("foreign/missing zero authority entered destructive barrier")
				}
			}
			if r.OVS.VNIRetirement != nil {
				t.Fatal("default-off fixture manufactured lifecycle callback")
			}
		})
	}
}
func TestResidualObserverZeroRetirementRejectsMissingAndForeignLease(t *testing.T) {
	for _, scenario := range []string{"missing-pool", "missing-generation", "wrong-pool", "wrong-owner", "foreign-lease-generation", "api-vni-missing"} {
		t.Run(scenario, func(t *testing.T) {
			r, _, a, _, _, binding := residualActualZeroLease(t, false)
			ctx := context.Background()
			barriers := 0
			r.Flows.client = scopeFlowAdapterWith(t, func() map[string]uint32 { return nil }, func() { barriers++ })
			switch scenario {
			case "missing-pool":
				binding.PoolUID = ""
			case "missing-generation":
				binding.LeaseGeneration = 0
			case "wrong-pool":
				binding.PoolUID = "foreign"
			case "wrong-owner":
				binding.OwnerUID = "foreign"
			case "foreign-lease-generation":
				binding.LeaseGeneration++
			case "api-vni-missing":
				a.Status.VNI = nil
				if err := r.Status().Update(ctx, &a); err != nil {
					t.Fatal(err)
				}
			}
			o := &NativeRuntimeObserver{Reader: r.Reader, NodeName: "node", BootID: "boot", JournalDir: t.TempDir()}
			r.OVS.vethMu.Lock()
			err := o.retireVNI(ctx, binding, r.Flows)
			r.OVS.vethMu.Unlock()
			if err == nil || barriers != 0 || o.vniReleased(binding) {
				t.Fatalf("invalid zero minted retirement: %v barriers=%d released=%v", err, barriers, o.vniReleased(binding))
			}
		})
	}
}
func TestResidualLegacyZeroDomainsUseExplicitMetadataAndRejectForeignZero(t *testing.T) {
	for _, scenario := range []string{"zero-success", "zero-to-foreign-domain", "foreign-zero-domain"} {
		t.Run(scenario, func(t *testing.T) {
			var r *ConnectionReconciler
			var conn *lab.Connection
			var a, b lab.Device
			var db *residualFabricDB
			if scenario == "foreign-zero-domain" {
				r, conn, a, b, db = residualFabricFixture(t, true)
			} else {
				r, conn, a, b, db, _ = residualActualZeroLease(t, true)
			}
			first := uint64(*a.Status.VNI)
			if scenario == "zero-to-foreign-domain" {
				first = 999
			}
			if scenario == "foreign-zero-domain" {
				first = 0
			}
			r.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
				return parseNativeFlows(fmt.Sprintf("table=0,in_port=7 actions=load:0x%x->metadata,drop\n table=0,in_port=8 actions=load:0x%x->metadata,drop", first, *b.Status.VNI))
			}
			err := r.migrateLegacySwitchPair(context.Background(), conn, a, b)
			if scenario == "zero-success" {
				if err != nil || db.deletes != 2 {
					t.Fatalf("actual explicit-zero migration refused: %v deletes=%d", err, db.deletes)
				}
			} else if err == nil || db.deletes != 0 {
				t.Fatalf("foreign zero flow domain accepted: %v deletes=%d", err, db.deletes)
			}
		})
	}
}
func TestResidualZeroScopeCompletesAfterActualLeaseBarriersWithOnlyDefaultDrop(t *testing.T) {
	r, _, a, b, _, binding := residualActualZeroLease(t, false)
	ctx := context.Background()
	_ = corev1.AddToScheme(r.Scheme())
	var ds lab.DeviceList
	var cs lab.ConnectionList
	var ls lab.LabList
	var ps allocation.PoolList
	for _, list := range []client.ObjectList{&ds, &cs, &ls, &ps} {
		if err := r.List(ctx, list); err != nil {
			t.Fatal(err)
		}
	}
	var objects []client.Object
	for i := range ds.Items {
		objects = append(objects, &ds.Items[i])
	}
	for i := range cs.Items {
		objects = append(objects, &cs.Items[i])
	}
	for i := range ls.Items {
		objects = append(objects, &ls.Items[i])
	}
	for i := range ps.Items {
		objects = append(objects, &ps.Items[i])
	}
	reader := fake.NewClientBuilder().WithScheme(r.Scheme()).WithIndex(&corev1.Pod{}, "spec.nodeName", func(object client.Object) []string { return []string{object.(*corev1.Pod).Spec.NodeName} }).WithObjects(objects...).Build()
	o, id := scopeProducerAdapter(t, nil, nil)
	o.Reader = reader
	id.ScopeKind = "LabFabric"
	id.ScopeUID = "lab"
	id.OwnerUID = "lab"
	id.LabName = "l"
	id.OperationID = binding.OperationID
	id.Revision = binding.Revision
	id.Generation = binding.Generation
	second := binding
	second.Name = b.Name
	second.UID = string(b.UID)
	second.VNI = *b.Status.VNI
	second.PoolUID = b.Status.VNILease.PoolUID
	second.LeaseGeneration = b.Status.VNILease.Generation
	id.VNIBindings = []lab.OwnedVNI{binding, second}
	o.Network.Flows.nativeFlowRead = func(context.Context) ([]nativeFlow, error) {
		return parseNativeFlows("table=0,priority=0 actions=drop")
	}
	report := o.ObserveScope(ctx, id)
	if report.RuntimeState != "Released" || report.Error != "" || report.AttachmentsAbsentAt == nil || !o.vniReleased(binding) || len(report.ReleasedVNIs) != 2 {
		t.Fatalf("zero/default-drop stranded full native scope: %+v", report)
	}
	if err := poolpkg.ValidateLease(ctx, reader, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: 0, PoolUID: a.Status.VNILease.PoolUID, OwnerUID: string(a.UID), Generation: a.Status.VNILease.Generation}); err != nil {
		t.Fatal("native proof itself released the Pool reservation", err)
	}
}
