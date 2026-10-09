package laboratory

import (
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Runtime scopes cover every actual registered placement node, without invented
// Pod/container identities. Old node obligations remain separately held.
func declaredScopes(ctx context.Context, reader client.Reader, owner, namespace, name, op string, revision, generation int64, kind string, placement ...nativePlacementPolicy) ([]lab.OwnedRuntimeIdentity, error) {
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("native placement node inventory unavailable")
	}
	policy := nativePlacementPolicy{}
	if len(placement) > 0 {
		policy = placement[0]
	}
	byName := map[string]*corev1.Node{}
	for i := range nodes.Items {
		byName[nodes.Items[i].Name] = &nodes.Items[i]
	}
	required, err := policy.requiredHistoricalNodes(owner, namespace, byName)
	if err != nil {
		return nil, err
	}
	var scopes []lab.OwnedRuntimeIdentity
	for _, node := range nodes.Items {
		linux := node.Status.NodeInfo.OperatingSystem == "linux" || node.Labels[corev1.LabelOSStable] == "linux" || node.Labels[names.LabelNodeAgentReady] == "true"
		eligible := required[node.Name] || linux && (configuredNativePlacement(&node, policy) || node.Labels[names.LabelNodeAgentReady] == "true")
		if !eligible {
			continue
		}
		if node.Labels[names.LabelNodeAgentReady] != "true" {
			return nil, fmt.Errorf("native placement node observer unavailable")
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			ready = ready || condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
		}
		if !ready || node.Status.NodeInfo.BootID == "" {
			return nil, fmt.Errorf("native placement node unavailable")
		}
		scopes = append(scopes, lab.OwnedRuntimeIdentity{ContainerIDs: []string{}, CgroupPaths: []string{}, PortKeys: []string{}, ScopeKind: kind, ScopeUID: owner, OwnerUID: owner, Namespace: namespace, LabName: name, OperationID: op, Revision: revision, Generation: generation, NodeName: node.Name, NodeBootID: node.Status.NodeInfo.BootID})
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("native Linux placement scope unavailable")
	}
	return scopes, nil
}
func (r *LabReconciler) prepareLabScopes(ctx context.Context, l *lab.Lab) error {
	if !r.RuntimeObservation {
		return nil
	}
	i := l.Spec.Lifecycle
	op, rev := "", int64(1)
	if i == nil {
		op = "legacy-native-" + string(l.UID) + "-" + fmt.Sprint(l.Generation)
	} else {
		op = i.OperationID
		rev = i.Revision
	}
	var devices lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &devices, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	var connections lab.ConnectionList
	if err := r.lifecycleReader().List(ctx, &connections, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	placement, err := r.labNativePlacement(ctx, l, devices.Items, connections.Items)
	if err != nil {
		return err
	}
	declared, err := declaredScopes(ctx, r.lifecycleReader(), string(l.UID), l.Namespace, l.Name, op, rev, l.Generation, "LabFabric", placement)
	if err != nil {
		return err
	}
	// Pin legacy reservations or recover a durable allocation whose object status
	// write was interrupted. No new reservation is created during stop preparation.
	for n := range devices.Items {
		d := &devices.Items[n]
		if objectOwnedBy(d, string(l.UID)) {
			if err := r.ensureOwnedVNI(ctx, d, false); err != nil {
				return err
			}
		}
	}
	for n := range connections.Items {
		c := &connections.Items[n]
		if objectOwnedBy(c, string(l.UID)) {
			if err := r.ensureOwnedVNI(ctx, c, false); err != nil {
				return err
			}
		}
	}
	bindings := []lab.OwnedVNI{}
	for _, d := range devices.Items {
		if objectOwnedBy(&d, string(l.UID)) && d.Status.VNI != nil {
			bindings = append(bindings, lab.OwnedVNI{PoolUID: d.Status.VNILease.PoolUID, LeaseGeneration: d.Status.VNILease.Generation, OwnerUID: string(l.UID), OperationID: op, Revision: rev, Generation: l.Generation, Kind: "Device", Namespace: d.Namespace, Name: d.Name, UID: string(d.UID), VNI: *d.Status.VNI})
		}
	}
	for _, c := range connections.Items {
		if objectOwnedBy(&c, string(l.UID)) && c.Status.VNI != nil {
			bindings = append(bindings, lab.OwnedVNI{PoolUID: c.Status.VNILease.PoolUID, LeaseGeneration: c.Status.VNILease.Generation, OwnerUID: string(l.UID), OperationID: op, Revision: rev, Generation: l.Generation, Kind: "Connection", Namespace: c.Namespace, Name: c.Name, UID: string(c.UID), VNI: *c.Status.VNI})
		}
	}
	baseDeclared := append([]lab.OwnedRuntimeIdentity(nil), declared...)
	for _, device := range devices.Items {
		never := objectOwnedBy(&device, string(l.UID)) && device.Spec.Type == lab.DeviceTypeContainer && device.Status.PodName == "" && device.Status.NodeName == "" && (device.Status.State == nil || device.Status.State.Incarnation == 0)
		if never {
			for _, base := range baseDeclared {
				scope := base
				scope.ScopeKind = "NeverMaterialized"
				scope.ScopeUID = string(device.UID)
				declared = append(declared, scope)
			}
		}
	}
	for n := range declared {
		if declared[n].ScopeKind == "NeverMaterialized" {
			continue
		}
		declared[n].VNIBindings = bindings
		for _, binding := range bindings {
			declared[n].VNIs = append(declared[n].VNIs, binding.VNI)
		}
	}
	rows := adoptReleasedScopeHistory(l.Status.ScopeInventory, l.Status.ScopeReports)
	for _, scope := range declared {
		for _, prior := range rows {
			if sameDeclaredScope(scope, prior) {
				for _, binding := range prior.VNIBindings {
					found := false
					for _, current := range scope.VNIBindings {
						found = found || reflect.DeepEqual(current, binding)
					}
					if !found {
						scope.VNIBindings = append(scope.VNIBindings, binding)
					}
				}
				scope.FabricPorts = append([]lab.OwnedFabricPort(nil), prior.FabricPorts...)
			}
		}
		for _, report := range l.Status.ScopeReports {
			if sameDeclaredScope(scope, report.Identity) && reflect.DeepEqual(scope.VNIBindings, report.Identity.VNIBindings) && report.Identity.AttachmentsComplete {
				scope = report.Identity
				break
			}
		}
		found := false
		for n, old := range rows {
			if sameDeclaredScope(scope, old) {
				rows[n] = scope
				found = true
				break
			}
		}
		if !found {
			rows = append(rows, scope)
		}
	}
	if reflect.DeepEqual(rows, l.Status.ScopeInventory) {
		return nil
	}
	base := l.DeepCopy()
	l.Status.ScopeInventory = rows
	return r.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
func sameDeclaredScope(a, b lab.OwnedRuntimeIdentity) bool {
	a.ContainerIDs, b.ContainerIDs = nil, nil
	a.CgroupPaths, b.CgroupPaths = nil, nil
	a.PortKeys, b.PortKeys = nil, nil
	a.PortRows, b.PortRows = nil, nil
	a.FabricPorts, b.FabricPorts = nil, nil
	a.VNIs, b.VNIs = nil, nil
	a.VNIBindings, b.VNIBindings = nil, nil
	a.AttachmentsComplete, b.AttachmentsComplete = false, false
	return reflect.DeepEqual(a, b)
}

// Return pool indices only after durable per-node native barriers. Retained
// declarations survive object deletion, so restart can finish the same release.
func (r *LabReconciler) releaseRecordedVNIs(ctx context.Context, l *lab.Lab) error {
	if !r.RuntimeObservation {
		return nil
	}

	seen := map[lab.OwnedVNI]bool{}
	var ds lab.DeviceList
	var cs lab.ConnectionList
	if err := r.lifecycleReader().List(ctx, &ds); err != nil {
		return err
	}
	if err := r.lifecycleReader().List(ctx, &cs); err != nil {
		return err
	}
	for _, scope := range l.Status.ScopeInventory {
		for _, binding := range scope.VNIBindings {
			if seen[binding] {
				continue
			}
			seen[binding] = true
			complete := true
			for _, history := range l.Status.ScopeInventory {
				for _, obligation := range history.VNIBindings {
					if obligation.UID == binding.UID && obligation.VNI == binding.VNI && obligation.PoolUID == binding.PoolUID && obligation.LeaseGeneration == binding.LeaseGeneration && !lab.VNILeaseReleased(l, obligation) {
						complete = false
					}
				}
			}
			if !complete {
				continue
			}
			var object client.Object
			switch binding.Kind {
			case "Device":
				object = &lab.Device{}
			case "Connection":
				object = &lab.Connection{}
			default:
				continue
			}
			err := r.lifecycleReader().Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, object)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && object.GetDeletionTimestamp().IsZero() {
				continue
			}
			reused := false
			for _, d := range ds.Items {
				reused = reused || d.Status.VNI != nil && *d.Status.VNI == binding.VNI && string(d.UID) != binding.UID
			}
			for _, c := range cs.Items {
				reused = reused || c.Status.VNI != nil && *c.Status.VNI == binding.VNI && string(c.UID) != binding.UID
			}
			if reused {
				continue
			}
			if err := poolpkg.ReleaseOwnedIndex(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: binding.VNI, PoolUID: binding.PoolUID, OwnerUID: binding.UID, Generation: binding.LeaseGeneration}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *LabReconciler) ensureOwnedVNI(ctx context.Context, object client.Object, allocate bool) error {
	var index *uint
	var current *lab.VNILease
	switch o := object.(type) {
	case *lab.Device:
		index = o.Status.VNI
		current = o.Status.VNILease
	case *lab.Connection:
		index = o.Status.VNI
		current = o.Status.VNILease
	default:
		return fmt.Errorf("unsupported lease owner")
	}
	if object.GetUID() == "" {
		return fmt.Errorf("actual VNI owner UID missing")
	}
	var lease *poolpkg.Lease
	var err error
	if index != nil {
		lease, err = poolpkg.PinExistingIndex(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, *index, string(object.GetUID()))
	} else if allocate {
		lease, err = poolpkg.AllocateOwnedIndex(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, string(object.GetUID()))
	} else {
		lease, err = poolpkg.FindOwnerLease(ctx, r.lifecycleReader(), names.VNIPoolPrefix, names.SystemNamespace, string(object.GetUID()))
	}
	if err != nil {
		return err
	}
	if lease == nil {
		return nil
	}
	if current != nil && current.PoolUID == lease.PoolUID && current.Generation == lease.Generation && index != nil && *index == lease.Index {
		return nil
	}
	next := &lab.VNILease{PoolUID: lease.PoolUID, Generation: lease.Generation}
	switch o := object.(type) {
	case *lab.Device:
		o.Status.VNI = &lease.Index
		o.Status.VNILease = next
	case *lab.Connection:
		o.Status.VNI = &lease.Index
		o.Status.VNILease = next
	}
	return r.Status().Update(ctx, object)
}

// Default-off compatibility keeps the legacy physical delete contract, but a
// pinned slot still requires the exact Pool UID, owner and lease generation.
// This does not produce any lifecycle release certificate.
func (r *LabReconciler) releaseDefaultOffVNI(ctx context.Context, index uint, lease *lab.VNILease, uid string) error {
	if lease != nil {
		if uid == "" {
			return fmt.Errorf("actual VNI owner UID missing")
		}
		var parents lab.LabList
		if err := r.lifecycleReader().List(ctx, &parents); err != nil {
			return err
		}
		for _, parent := range parents.Items {
			for _, scope := range parent.Status.ScopeInventory {
				for _, binding := range scope.VNIBindings {
					if binding.UID == uid && !lab.VNILeaseReleased(&parent, binding) {
						return fmt.Errorf("retained native VNI obligation is unresolved")
					}
				}
			}
		}
		return poolpkg.ReleaseOwnedIndex(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: index, PoolUID: lease.PoolUID, OwnerUID: uid, Generation: lease.Generation})
	}
	return poolpkg.NewRotatingAllocator(r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize).ReleaseIndex(ctx, index)
}

// Historical declarations may gain positive native inventory after Start was
// accepted. Only a complete release certificate extending every prior debt is
// adopted; consumption still uses exact full identity equality.
func adoptReleasedScopeHistory(rows []lab.OwnedRuntimeIdentity, reports []lab.OwnedRuntimeReport) []lab.OwnedRuntimeIdentity {
	out := append([]lab.OwnedRuntimeIdentity(nil), rows...)
	for i, prior := range out {
		if prior.ScopeKind != "LabFabric" && prior.ScopeKind != "GroupScope" && prior.ScopeKind != "NeverMaterialized" {
			continue
		}
		for _, report := range reports {
			next := report.Identity
			if !sameDeclaredScope(prior, next) || !scopeHistoryContains(next.ContainerIDs, prior.ContainerIDs) || !scopeHistoryContains(next.CgroupPaths, prior.CgroupPaths) || !scopeHistoryContains(next.PortKeys, prior.PortKeys) || !scopeHistoryContains(next.PortRows, prior.PortRows) || !scopeHistoryContains(next.FabricPorts, prior.FabricPorts) || !scopeHistoryContains(next.VNIs, prior.VNIs) || !scopeHistoryContains(next.VNIBindings, prior.VNIBindings) {
				continue
			}
			if !runtimeRowsReleased([]lab.OwnedRuntimeIdentity{next}, []lab.OwnedRuntimeReport{report}, next.OwnerUID, next.OperationID, next.Revision) {
				continue
			}
			out[i] = *next.DeepCopy()
			prior = out[i]
		}
	}
	return out
}
func scopeHistoryContains[T comparable](all, prior []T) bool {
	for _, old := range prior {
		found := false
		for _, item := range all {
			found = found || item == old
		}
		if !found {
			return false
		}
	}
	return true
}
