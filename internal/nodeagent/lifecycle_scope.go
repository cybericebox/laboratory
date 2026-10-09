//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (o *NativeRuntimeObserver) scopeCurrent(ctx context.Context, id lab.OwnedRuntimeIdentity, persistentDebt ...*lab.OwnedRuntimeIdentity) (bool, error) {
	if scopeObservationIdentityInvalid(o, id) {
		return false, ErrPortOwnerUnknown
	}
	if id.ScopeKind == scopeLabFabric || id.ScopeKind == runtimeNeverMaterialized {
		return o.labScopeCurrent(ctx, id, persistentDebt...)
	}
	var groups lab.LabGroupList
	if err := o.Reader.List(ctx, &groups); err != nil {
		return false, err
	}
	for _, g := range groups.Items {
		if string(g.UID) == id.OwnerUID {
			if lab.LabGroupNamespaceOf(&g) != id.Namespace || g.Spec.Lifecycle == nil {
				return false, ErrPortOwnerChanged
			}
			current := g.Generation == id.Generation && g.Spec.Lifecycle.OperationID == id.OperationID && g.Spec.Lifecycle.Revision == id.Revision
			if !current {
				found := false
				for _, row := range g.Status.ServiceRuntime {
					found = found || sameDeclaredNativeScope(row, id)
				}
				if !found {
					return false, ErrPortOwnerChanged
				}
			}
			return g.Spec.Lifecycle.IsStopped() || !current, nil
		}
	}
	return false, ErrPortOwnerUnknown
}

func historicalNeverMaterializedPodProof(l *lab.Lab, d *lab.Device, id lab.OwnedRuntimeIdentity, operation string, revision int64) bool {
	// This exception is only for a genuinely empty ordinary Deployment
	// declaration. Positive historical scopes and persistence incarnations keep
	// their existing boundary; no native debt is discarded or rebound.
	emptyDeclaration := false
	for _, declared := range l.Status.ScopeInventory {
		if !sameDeclaredNativeScope(declared, id) {
			continue
		}
		// Rechecks receive native-enriched identities. The original retained
		// declaration is the emptiness authority, including at the final fence.
		if scopeDeclarationHasRuntime(declared) {
			return false
		}
		emptyDeclaration = true
	}
	if !emptyDeclaration {
		return false
	}
	if historicalOrdinaryDeviceInvalid(id, l, revision, d) {
		return false
	}
	owned := false
	for _, parent := range d.OwnerReferences {
		owned = owned || parent.Kind == ownerKindLab && parent.Name == l.Name && parent.UID == l.UID
	}
	if !owned {
		return false
	}
	for _, report := range d.Status.RuntimeReports {
		proof := report.Identity
		if historicalOrdinaryReportInvalid(proof, id, operation, revision, l, d, report) {
			continue
		}
		if report.RuntimeState == "Allocated" || report.RuntimeState == "Present" || committedRuntimeReport(report, proof) {
			return true
		}
	}
	return false
}

// A committed persistent writer can bind its retained empty pre-Pod declaration
// only through current capture and exact positive native release history. This
// admits a rescan of the known physical debt; it is never an absence certificate.
func persistentHistoricalNeverMaterializedPodProof(l *lab.Lab, d *lab.Device, id lab.OwnedRuntimeIdentity, operation string, revision int64) (lab.OwnedRuntimeIdentity, bool) {
	var none lab.OwnedRuntimeIdentity
	if historicalPersistentIntentInvalid(id, l, revision) {
		return none, false
	}
	barrier := l.Status.Lifecycle
	if historicalPersistentBarrierInvalid(barrier, l, operation, revision) {
		return none, false
	}
	emptyDeclaration := false
	for _, declared := range l.Status.ScopeInventory {
		if !sameDeclaredNativeScope(declared, id) {
			continue
		}
		if scopeDeclarationHasRuntime(declared) {
			return none, false
		}
		emptyDeclaration = true
	}
	state := d.Status.State
	if historicalPersistentDeviceInvalid(emptyDeclaration, d, state, id, l) {
		return none, false
	}
	owned := false
	for _, parent := range d.OwnerReferences {
		owned = owned || parent.Kind == ownerKindLab && parent.Name == l.Name && parent.UID == l.UID
	}
	capture := state.Capture
	if historicalPersistentCaptureInvalid(owned, capture, operation, revision, state) {
		return none, false
	}
	for _, proof := range d.Status.RuntimeInventory {
		if historicalPersistentRuntimeInvalid(proof, id, operation, revision, l, state, capture) {
			continue
		}
		for _, report := range d.Status.RuntimeReports {
			if committedRuntimeReport(report, proof) {
				return proof, true
			}
		}
	}
	return none, false
}

func envValue(env []string, key string) string {
	for _, value := range env {
		if strings.HasPrefix(value, key+"=") {
			return strings.TrimPrefix(value, key+"=")
		}
	}
	return ""
}
func (o *NativeRuntimeObserver) ObserveScope(ctx context.Context, id lab.OwnedRuntimeIdentity, fresh ...bool) lab.OwnedRuntimeReport {
	o.mu.Lock()
	defer o.mu.Unlock()
	id = runtimeWireIdentity(id)
	var committed lab.OwnedRuntimeReport
	priorCommitted := false
	if o.readRecord("scope-fabric-released", id, &committed) == nil {
		committed.Identity = runtimeWireIdentity(committed.Identity)
		priorCommitted = committedRuntimeReport(committed, id)
		if (len(fresh) == 0 || !fresh[0]) && priorCommitted {
			return committed
		}
	}
	retirement := o.currentScopeRetirement(ctx, id, fresh)
	now := metav1.Now()
	out := lab.OwnedRuntimeReport{Identity: id, RuntimeState: "Unknown", ObservedAt: &now}
	fail := func(err error) lab.OwnedRuntimeReport { out.Error = err.Error(); return out }
	var persistentDebt lab.OwnedRuntimeIdentity
	scopeFence := func(identity lab.OwnedRuntimeIdentity) (bool, error) {
		stopped, err := o.scopeCurrent(ctx, identity, &persistentDebt)
		if retirement != nil {
			if fenceErr := o.retirementScopeCurrent(ctx, identity, *retirement); fenceErr != nil {
				return false, fenceErr
			}
			// The exact old certificate authorizes its original positive debt for
			// a full fresh retirement scan, never a copied challenge acknowledgement.
			if err != nil && priorCommitted {
				return true, nil
			}
		}
		return stopped, err
	}
	stopped, err := scopeFence(id)
	if err != nil {
		return fail(err)
	}
	if persistentDebt.PodUID != "" {
		// Preserve the historical declaration tuple while scanning every positively
		// bound physical obligation, even after the live Pod/journal disappeared.
		mergeNativeScopeIdentity(&id, persistentDebt)
	}

	var saved lab.OwnedRuntimeIdentity
	if err := o.readRecord("scope", id, &saved); err == nil {
		if !sameDeclaredNativeScope(id, saved) {
			return fail(ErrPortOwnerChanged)
		}
		mergeNativeScopeIdentity(&id, saved)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	owners, err := o.nativePodInventories()
	if err != nil {
		return fail(err)
	}
	for _, owner := range owners {
		if scopeOwnsPod(id, owner) {
			mergeNativeScopeIdentity(&id, owner)
		}
	}
	native := namespaces.WithNamespace(ctx, o.Namespace)
	containers, err := o.Runtime.Containers(native)
	if err != nil {
		return fail(err)
	}
	metadata := map[string]bool{}
	knownPods := map[string]string{}
	var currentPods corev1.PodList
	if err := o.Reader.List(ctx, &currentPods, client.MatchingFields{"spec.nodeName": o.NodeName}); err != nil {
		return fail(err)
	}
	for _, pod := range currentPods.Items {
		knownPods[string(pod.UID)] = pod.Namespace
	}
	owned := append([]string{}, id.ContainerIDs...)
	cgroups := append([]string{}, id.CgroupPaths...)
	owned, cgroups, err = o.collectScopeContainers(ctx, native, id, owners, containers, metadata, knownPods, owned, cgroups)
	if err != nil {
		return fail(err)
	}
	// Save positive history before any later task/fabric/cgroup scan can fail.
	id.ContainerIDs = append([]string{}, owned...)
	id.CgroupPaths = append([]string{}, cgroups...)
	id.AttachmentsComplete = false
	if err := o.writeRecord("scope", id, id); err != nil {
		return fail(err)
	}
	out.Identity = id
	if err := o.captureScopeCgroups(&id, knownPods, owners); err != nil {
		return fail(err)
	}
	for _, path := range id.CgroupPaths {
		if !runtimeContainsID(cgroups, path) {
			cgroups = append(cgroups, path)
		}
	}
	id.ContainerIDs = append([]string{}, owned...)
	id.CgroupPaths = append([]string{}, cgroups...)
	out.Identity = id
	if err := o.writeRecord("scope", id, id); err != nil {
		return fail(err)
	}
	if len(owned) > 0 && len(cgroups) == 0 {
		return fail(fmt.Errorf("owned scope cgroup inventory incomplete"))
	}
	done, err := o.observeScopePresence(ctx, native, &id, &out, owners, metadata, owned, cgroups, now)
	if err != nil {
		return fail(err)
	}
	if done {
		return out
	}
	return o.observeAbsentScope(ctx, id, out, now, stopped, owners, retirement, scopeFence)
}

func (o *NativeRuntimeObserver) retirementScopeCurrent(ctx context.Context, id lab.OwnedRuntimeIdentity, expected lab.LifecycleRetirementIntent) error {
	if id.NodeName != o.NodeName || id.NodeBootID != o.BootID || o.BootID == "" {
		return ErrPortOwnerChanged
	}
	var parent lab.Lab
	if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.LabName}, &parent); err != nil {
		return err
	}
	valid := false
	for _, declared := range parent.Status.ScopeInventory {
		current, bound := nativeLabRetirementChallenge(&parent, declared, parent.Status.ScopeInventory)
		// Intermediate scans can add physical debt and clear completion while
		// they retire it. No original obligation or stable identity can disappear.
		if bound && reflect.DeepEqual(current, expected) && sameDeclaredNativeScope(declared, id) && retirementDebtContains(id.ContainerIDs, declared.ContainerIDs) && retirementDebtContains(id.CgroupPaths, declared.CgroupPaths) && retirementDebtContains(id.PortKeys, declared.PortKeys) && retirementDebtContains(id.PortRows, declared.PortRows) && retirementDebtContains(id.FabricPorts, declared.FabricPorts) && retirementDebtContains(id.VNIs, declared.VNIs) && retirementDebtContains(id.VNIBindings, declared.VNIBindings) {
			valid = true
			break
		}
	}
	if !valid {
		return ErrPortOwnerChanged
	}
	if id.ScopeKind == runtimeNeverMaterialized {
		var devices lab.DeviceList
		if err := o.Reader.List(ctx, &devices, client.InNamespace(id.Namespace)); err != nil {
			return err
		}
		for _, d := range devices.Items {
			if string(d.UID) != id.ScopeUID || d.Spec.LabRef != parent.Name || d.Status.NodeName != "" && d.Status.NodeName != id.NodeName {
				continue
			}
			for _, owner := range d.OwnerReferences {
				if owner.Kind == ownerKindLab && owner.Name == parent.Name && owner.UID == parent.UID {
					return nil
				}
			}
		}
		return ErrPortOwnerChanged
	}
	return nil
}

func retirementDebtContains[T any](observed, declared []T) bool {
	for _, debt := range declared {
		found := false
		for _, actual := range observed {
			found = found || reflect.DeepEqual(actual, debt)
		}
		if !found {
			return false
		}
	}
	return true
}
func (o *NativeRuntimeObserver) captureScopeFabric(ctx context.Context, id *lab.OwnedRuntimeIdentity) error {
	if id.ScopeKind == scopeGroup || id.ScopeKind == runtimeNeverMaterialized {
		return nil
	}
	var devices lab.DeviceList
	if err := o.Reader.List(ctx, &devices, client.InNamespace(id.Namespace)); err != nil {
		return err
	}
	add := func(vni uint) {
		for _, old := range id.VNIs {
			if old == vni {
				return
			}
		}
		id.VNIs = append(id.VNIs, vni)
	}
	for _, binding := range id.VNIBindings {
		if o.vniReleased(binding) {
			continue
		}
		found := false
		if binding.Kind == ownerKindDevice {
			for _, d := range devices.Items {
				if deviceBindingCurrent(d, binding) {
					found = true
				}
			}
		}
		if binding.Kind == ownerKindDevice && !found {
			return fmt.Errorf("original Device VNI lease owner unavailable or replaced")
		}
		add(binding.VNI)
	}
	var connections lab.ConnectionList
	if err := o.Reader.List(ctx, &connections, client.InNamespace(id.Namespace)); err != nil {
		return err
	}
	for _, binding := range id.VNIBindings {
		if binding.Kind == ownerKindConnection && !o.vniReleased(binding) {
			found := false
			for _, c := range connections.Items {
				if connectionBindingCurrent(c, binding) {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("original Connection VNI lease owner unavailable or replaced")
			}
		}
	}
	if err := o.captureConnectionFabric(id, connections.Items); err != nil {
		return err
	}
	for _, binding := range id.VNIBindings {
		if o.vniReleased(binding) {
			continue
		}
		count := 0
		for _, d := range devices.Items {
			if d.Status.VNI != nil && *d.Status.VNI == binding.VNI {
				count++
			}
		}
		for _, c := range connections.Items {
			if c.Status.VNI != nil && *c.Status.VNI == binding.VNI {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("VNI lease owner is not unique")
		}
	}
	return nil
}

type fabricReceipt struct {
	ConnectionUID, LabUID, Namespace, LabName, OperationID, RowUUID, Key string
	Revision, Generation                                                 int64
}

func (o *NativeRuntimeObserver) prepareFabric(key string, uid types.UID, row string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var connections lab.ConnectionList
	if err := o.Reader.List(ctx, &connections); err != nil {
		return err
	}
	for _, conn := range connections.Items {
		if conn.UID != uid {
			continue
		}
		var parent lab.Lab
		if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: conn.Namespace, Name: conn.Spec.LabRef}, &parent); err != nil {
			return err
		}
		owned := false
		for _, reference := range conn.OwnerReferences {
			owned = owned || reference.Kind == ownerKindLab && reference.UID == parent.UID
		}
		if !owned {
			return ErrPortOwnerChanged
		}
		op, rev := nativeLabOperation(&parent)
		receipt := fabricReceipt{ConnectionUID: string(uid), LabUID: string(parent.UID), Namespace: conn.Namespace, LabName: parent.Name, OperationID: op, Revision: rev, Generation: parent.Generation, RowUUID: row, Key: key}
		return o.writeRecord("fabric-inventory-"+key, lab.OwnedRuntimeIdentity{ScopeUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, receipt)
	}
	return ErrPortOwnerUnknown
}
func (o *NativeRuntimeObserver) commitFabric(key string, uid types.UID, row string) error {
	id := lab.OwnedRuntimeIdentity{ScopeUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}
	var receipt fabricReceipt
	if err := o.readRecord("fabric-inventory-"+key, id, &receipt); err != nil {
		return err
	}
	if receipt.ConnectionUID != string(uid) || receipt.Key != key || receipt.RowUUID != row {
		return ErrPortOwnerChanged
	}
	return o.writeRecord("fabric-released-"+key, id, receipt)
}
func (o *NativeRuntimeObserver) fabricAbsent(key string, uid types.UID) error {
	var receipt fabricReceipt
	if err := o.readRecord("fabric-released-"+key, lab.OwnedRuntimeIdentity{ScopeUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, &receipt); err != nil {
		return err
	}
	if receipt.ConnectionUID != string(uid) || receipt.Key != key || receipt.RowUUID == "" {
		return ErrPortOwnerUnknown
	}
	return nil
}
func (o *NativeRuntimeObserver) portAbsent(key string, uid types.UID) error {
	var id lab.OwnedRuntimeIdentity
	if err := o.readRecord("owner", lab.OwnedRuntimeIdentity{PodUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, &id); err != nil {
		return err
	}
	var receipt cleanupReceipt
	if err := o.readRecord("cleanup-"+key, id, &receipt); err != nil {
		return err
	}
	if receipt.PortUUID == "" || !reflect.DeepEqual(receipt.Identity, id) {
		return ErrPortOwnerUnknown
	}
	return nil
}

type vniReceipt struct {
	Binding     lab.OwnedVNI `json:"binding"`
	LabUID      string       `json:"labUID"`
	OperationID string       `json:"operationID"`
	Revision    int64        `json:"revision"`
	Generation  int64        `json:"generation"`
	Complete    bool         `json:"complete"`
}

func (o *NativeRuntimeObserver) vniIdentity(binding lab.OwnedVNI) lab.OwnedRuntimeIdentity {
	return lab.OwnedRuntimeIdentity{ScopeKind: "VNI", DeploymentUID: binding.PoolUID, ScopeUID: binding.UID, OwnerUID: binding.OwnerUID, OperationID: binding.OperationID, Revision: binding.Revision, Generation: binding.LeaseGeneration, NodeName: o.NodeName, NodeBootID: o.BootID}
}
func (o *NativeRuntimeObserver) vniReleased(binding lab.OwnedVNI) bool {
	var receipt vniReceipt
	return o.readRecord("vni-released", o.vniIdentity(binding), &receipt) == nil && receipt.Complete && reflect.DeepEqual(receipt.Binding, binding)
}

// Caller holds the common physical creation/retirement lock. Recovery consumes
// a completed old lease receipt without deleting any later owner's numeric VNI.
func (o *NativeRuntimeObserver) retireVNI(ctx context.Context, binding lab.OwnedVNI, flows *FlowManager) error {
	if binding.VNI == 0 && !completeZeroVNIBinding(binding) {
		return ErrPortOwnerUnknown
	}
	if o.vniReleased(binding) {
		return nil
	}
	if binding.VNI == 0 {
		if _, err := validateZeroVNIBinding(ctx, o.Reader, binding); err != nil {
			return err
		}
	}
	if err := poolpkg.ValidateLease(ctx, o.Reader, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, poolpkg.Lease{Index: binding.VNI, PoolUID: binding.PoolUID, OwnerUID: binding.UID, Generation: binding.LeaseGeneration}); err != nil {
		return err
	}
	if o.Reader == nil || binding.UID == "" {
		return ErrPortOwnerUnknown
	}
	var object client.Object
	switch binding.Kind {
	case ownerKindConnection:
		object = &lab.Connection{}
	case ownerKindDevice:
		object = &lab.Device{}
	default:
		return ErrPortOwnerUnknown
	}
	if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, object); err != nil {
		return err
	}
	if string(object.GetUID()) != binding.UID {
		return ErrPortOwnerChanged
	}
	var actual *uint
	var labName string
	switch current := object.(type) {
	case *lab.Connection:
		actual = current.Status.VNI
		labName = current.Spec.LabRef
	case *lab.Device:
		actual = current.Status.VNI
		labName = current.Spec.LabRef
	}
	if actual == nil || *actual != binding.VNI {
		return ErrPortOwnerChanged
	}
	var parent lab.Lab
	if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: labName}, &parent); err != nil {
		return err
	}
	owned := vniObjectOwned(object, &parent)
	if !owned || binding.OwnerUID != string(parent.UID) {
		return ErrPortOwnerChanged
	}
	if err := o.uniqueVNI(ctx, binding.VNI); err != nil {
		return err
	}
	op, rev := nativeLabOperation(&parent)
	receipt := vniReceipt{Binding: binding, LabUID: string(parent.UID), OperationID: op, Revision: rev, Generation: parent.Generation}
	if err := o.writeRecord("vni-inventory", o.vniIdentity(binding), receipt); err != nil {
		return err
	}
	if err := flows.RetireVNI(binding.VNI); err != nil {
		return err
	}
	// API object replacement during a barrier cannot authorize a completed ACK.
	fresh := object.DeepCopyObject().(client.Object)
	if err := o.Reader.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
		return err
	}
	if fresh.GetUID() != object.GetUID() || fresh.GetResourceVersion() != object.GetResourceVersion() {
		return ErrPortOwnerChanged
	}
	if binding.VNI == 0 {
		if _, err := validateZeroVNIBinding(ctx, o.Reader, binding); err != nil {
			return err
		}
	}
	receipt.Complete = true
	return o.writeRecord("vni-released", o.vniIdentity(binding), receipt)
}

func nativeGroupDeploymentOwned(dep *appsv1.Deployment, uid string) bool {
	for _, container := range dep.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "GROUP_UID" && env.Value == uid {
				return true
			}
		}
	}
	return false
}

func sameDeclaredNativeScope(a, b lab.OwnedRuntimeIdentity) bool {
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

func scopeObservationIdentityInvalid(o *NativeRuntimeObserver, id lab.OwnedRuntimeIdentity) bool {
	return o.Reader == nil || id.NodeName != o.NodeName || id.NodeBootID != o.BootID || id.ScopeUID == "" || id.ScopeKind != runtimeNeverMaterialized && id.ScopeUID != id.OwnerUID || id.Generation < 1
}

func scopeDeclarationHasRuntime(declared lab.OwnedRuntimeIdentity) bool {
	return declared.PodUID != "" || declared.DeploymentUID != "" || declared.Component != "" || declared.Epoch != 0 || declared.Incarnation != 0 || len(declared.ContainerIDs) != 0 || len(declared.CgroupPaths) != 0 || len(declared.PortKeys) != 0 || len(declared.PortRows) != 0 || len(declared.FabricPorts) != 0 || len(declared.VNIs) != 0 || len(declared.VNIBindings) != 0 || declared.Requests != (lab.ResourceAmounts{}) || declared.Limits != (lab.ResourceAmounts{})
}

func historicalOrdinaryDeviceInvalid(id lab.OwnedRuntimeIdentity, l *lab.Lab, revision int64, d *lab.Device) bool {
	return id.Generation >= l.Generation || id.Revision >= revision || d.Spec.Type != lab.DeviceTypeContainer || d.Spec.State != nil && d.Spec.State.Enabled || d.Status.State != nil || d.Spec.LabRef != l.Name || d.Namespace != id.Namespace || string(d.UID) != id.ScopeUID
}

func historicalOrdinaryReportInvalid(proof lab.OwnedRuntimeIdentity, id lab.OwnedRuntimeIdentity, operation string, revision int64, l *lab.Lab, d *lab.Device, report lab.OwnedRuntimeReport) bool {
	return proof.ScopeKind != "" && proof.ScopeKind != "Pod" || proof.OwnerUID != id.OwnerUID || proof.ScopeUID != id.ScopeUID || proof.Namespace != id.Namespace || proof.LabName != id.LabName || proof.OperationID != operation || proof.Revision != revision || proof.Generation != l.Generation || proof.NodeName != id.NodeName || proof.NodeName != d.Status.NodeName || proof.NodeBootID != id.NodeBootID || proof.Epoch != id.Epoch || proof.Incarnation != id.Incarnation || proof.PodUID == "" || len(proof.ContainerIDs) == 0 || len(proof.CgroupPaths) == 0 || !proof.AttachmentsComplete || report.Error != "" || report.ObservedAt == nil || report.ObservedAt.IsZero()
}

func historicalPersistentIntentInvalid(id lab.OwnedRuntimeIdentity, l *lab.Lab, revision int64) bool {
	return id.ScopeKind != runtimeNeverMaterialized || id.Generation >= l.Generation || id.Revision >= revision || l.Spec.Lifecycle == nil || !l.Spec.Lifecycle.IsStopped() || l.Spec.Lifecycle.SnapshotMode != "Required"
}

func historicalPersistentBarrierInvalid(barrier *lab.LabLifecycleStatus, l *lab.Lab, operation string, revision int64) bool {
	return barrier == nil || !barrier.SnapshotComplete || barrier.LabUID != string(l.UID) || barrier.OperationID != operation || barrier.Revision != revision || barrier.ObservedGeneration != l.Generation
}

func historicalPersistentDeviceInvalid(emptyDeclaration bool, d *lab.Device, state *lab.DeviceStateStatus, id lab.OwnedRuntimeIdentity, l *lab.Lab) bool {
	return !emptyDeclaration || d.Spec.Type != lab.DeviceTypeContainer || !d.Spec.StateEnabled() || state == nil || state.Epoch != id.Epoch || state.Incarnation != 1 || d.Spec.LabRef != l.Name || d.Namespace != id.Namespace || string(d.UID) != id.ScopeUID || d.Status.NodeName != id.NodeName
}

func historicalPersistentCaptureInvalid(owned bool, capture *lab.DeviceCaptureResult, operation string, revision int64, state *lab.DeviceStateStatus) bool {
	return !owned || capture == nil || capture.OperationID != operation || capture.LifecycleRevision != revision || capture.Epoch != state.Epoch || capture.Incarnation != state.Incarnation || capture.PodUID == "" || capture.NodeAgentEpoch == "" || capture.Result != "Succeeded" || !capture.Quiesced || capture.GuardState != "Held" || !capture.Committed
}

func historicalPersistentRuntimeInvalid(proof lab.OwnedRuntimeIdentity, id lab.OwnedRuntimeIdentity, operation string, revision int64, l *lab.Lab, state *lab.DeviceStateStatus, capture *lab.DeviceCaptureResult) bool {
	return proof.ScopeKind != "" && proof.ScopeKind != "Pod" || proof.OwnerUID != id.OwnerUID || proof.ScopeUID != id.ScopeUID || proof.Namespace != id.Namespace || proof.LabName != id.LabName || proof.OperationID != operation || proof.Revision != revision || proof.Generation != l.Generation || proof.NodeName != id.NodeName || proof.NodeBootID != id.NodeBootID || proof.Epoch != state.Epoch || proof.Incarnation != state.Incarnation || proof.PodUID != capture.PodUID || proof.DeploymentUID != "" || proof.Component != "" || len(proof.ContainerIDs) == 0 || len(proof.CgroupPaths) == 0 || !proof.AttachmentsComplete
}

func deviceBindingCurrent(d lab.Device, binding lab.OwnedVNI) bool {
	return d.Namespace == binding.Namespace && d.Name == binding.Name && string(d.UID) == binding.UID && d.Status.VNI != nil && *d.Status.VNI == binding.VNI
}

func connectionBindingCurrent(c lab.Connection, binding lab.OwnedVNI) bool {
	return c.Namespace == binding.Namespace && c.Name == binding.Name && string(c.UID) == binding.UID && c.Status.VNI != nil && *c.Status.VNI == binding.VNI
}

func (o *NativeRuntimeObserver) labScopeCurrent(ctx context.Context, id lab.OwnedRuntimeIdentity, persistentDebt ...*lab.OwnedRuntimeIdentity) (bool, error) {
	var l lab.Lab
	if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.LabName}, &l); err != nil {
		return false, err
	}
	if string(l.UID) != id.OwnerUID {
		return false, ErrPortOwnerChanged
	}
	operation, revision := nativeLabOperation(&l)
	current := l.Generation == id.Generation && operation == id.OperationID && revision == id.Revision
	if !current {
		retained := false
		for _, old := range l.Status.ScopeInventory {
			retained = retained || sameDeclaredNativeScope(old, id)
		}
		if !retained {
			return false, ErrPortOwnerChanged
		}
	}
	if id.ScopeKind == runtimeNeverMaterialized {
		var devices lab.DeviceList
		if err := o.Reader.List(ctx, &devices, client.InNamespace(id.Namespace)); err != nil {
			return false, err
		}
		found := false
		for _, d := range devices.Items {
			if string(d.UID) == id.ScopeUID {
				found = d.Status.PodName == "" && d.Status.NodeName == "" && (d.Status.State == nil || d.Status.State.Incarnation == 0)
				// An exact retained pre-Pod declaration can outlive ordinary
				// Deployment materialization. A later bound native Pod proof
				// admits only the full native observation path, never release.
				found = found || !current && historicalNeverMaterializedPodProof(&l, &d, id, operation, revision)
				if !current {
					if proof, ok := persistentHistoricalNeverMaterializedPodProof(&l, &d, id, operation, revision); ok {
						found = true
						if len(persistentDebt) > 0 && persistentDebt[0] != nil {
							*persistentDebt[0] = proof
						}
					}
				}
			}
		}
		if !found {
			return false, ErrPortOwnerChanged
		}
	}
	return !l.DeletionTimestamp.IsZero() || l.Spec.Lifecycle != nil && l.Spec.Lifecycle.IsStopped() || !current, nil
}

func (o *NativeRuntimeObserver) legacyScopeContainerOwned(ctx context.Context, id lab.OwnedRuntimeIdentity, containerID string, labels map[string]string, owners []lab.OwnedRuntimeIdentity) (bool, error) {
	match := false
	// A current API owner may attribute legacy runtime; absent APIs never do.
	attributed := false
	for _, prior := range owners {
		if prior.PodUID == labels["io.kubernetes.pod.uid"] && runtimeContainsID(prior.ContainerIDs, containerID) {
			attributed = true
			match = scopeOwnsPod(id, prior)
		}
	}
	if attributed {
		if !match {
			return false, nil
		}
	} else {
		var pod corev1.Pod
		if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: labels["io.kubernetes.pod.name"]}, &pod); err != nil {
			return false, fmt.Errorf("unattributed legacy native runtime in scope")
		}
		if string(pod.UID) != labels["io.kubernetes.pod.uid"] {
			return false, ErrPortOwnerChanged
		}
		for _, reference := range pod.OwnerReferences {
			if id.ScopeKind == scopeGroup && reference.Kind == ownerKindReplicaSet {
				var rs appsv1.ReplicaSet
				if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: reference.Name}, &rs); err != nil {
					return false, err
				}
				if rs.UID != reference.UID {
					return false, ErrPortOwnerChanged
				}
				for _, parent := range rs.OwnerReferences {
					if parent.Kind == "Deployment" {
						var dep appsv1.Deployment
						if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: parent.Name}, &dep); err != nil {
							return false, err
						}
						if dep.UID != parent.UID {
							return false, ErrPortOwnerChanged
						}
						match = match || nativeGroupDeploymentOwned(&dep, id.OwnerUID)
					}
				}
			}
			if reference.Kind == ownerKindDevice {
				var d lab.Device
				if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: reference.Name}, &d); err != nil {
					return false, err
				}
				if d.UID != reference.UID {
					return false, ErrPortOwnerChanged
				}
				for _, parent := range d.OwnerReferences {
					match = match || (id.ScopeKind == scopeLabFabric || id.ScopeKind == runtimeNeverMaterialized && string(d.UID) == id.ScopeUID) && parent.Kind == ownerKindLab && string(parent.UID) == id.OwnerUID
				}
			}
		}
	}
	return match, nil
}

func (o *NativeRuntimeObserver) collectScopeContainers(ctx, native context.Context, id lab.OwnedRuntimeIdentity, owners []lab.OwnedRuntimeIdentity, containers []containerd.Container, metadata map[string]bool, knownPods map[string]string, owned, cgroups []string) ([]string, []string, error) {
	for _, container := range containers {
		info, err := container.Info(native)
		if err != nil {
			return owned, cgroups, err
		}
		metadata[container.ID()] = true
		if uid := info.Labels["io.kubernetes.pod.uid"]; uid != "" {
			knownPods[uid] = info.Labels["io.kubernetes.pod.namespace"]
		}
		if info.Labels["io.kubernetes.pod.namespace"] != id.Namespace {
			continue
		}
		spec, err := container.Spec(native)
		if err != nil {
			return owned, cgroups, err
		}
		owner := ""
		group := ""
		if spec.Process != nil {
			owner = envValue(spec.Process.Env, "LIFECYCLE_LAB_UID")
			group = envValue(spec.Process.Env, "GROUP_UID")
		}
		deviceOwner := ""
		if spec.Process != nil {
			deviceOwner = envValue(spec.Process.Env, "LIFECYCLE_DEVICE_UID")
		}
		match := id.ScopeKind == scopeLabFabric && owner == id.OwnerUID || id.ScopeKind == scopeGroup && group == id.OwnerUID || id.ScopeKind == runtimeNeverMaterialized && owner == id.OwnerUID && deviceOwner == id.ScopeUID
		if !match && owner == "" && group == "" {
			match, err = o.legacyScopeContainerOwned(ctx, id, container.ID(), info.Labels, owners)
			if err != nil {
				return owned, cgroups, err
			}
		}
		if !match {
			continue
		}
		if !runtimeContainsID(owned, container.ID()) {
			owned = append(owned, container.ID())
		}
		if spec.Linux != nil {
			path := devicestate.CgroupDir(o.CgroupRoot, spec.Linux.CgroupsPath)
			if path != "" {
				if !runtimeContainsID(cgroups, path) {
					cgroups = append(cgroups, path)
				}
			}
		}
	}
	return owned, cgroups, nil
}

func (o *NativeRuntimeObserver) observeScopePresence(ctx, native context.Context, id *lab.OwnedRuntimeIdentity, out *lab.OwnedRuntimeReport, owners []lab.OwnedRuntimeIdentity, metadata map[string]bool, owned, cgroups []string, now metav1.Time) (bool, error) {
	list, err := o.Runtime.TaskService().List(native, &tasks.ListTasksRequest{})
	if err != nil {
		return false, err
	}
	for _, row := range list.Tasks {
		if row == nil {
			continue
		}
		key := row.ContainerID
		if key == "" {
			key = row.ID
		}
		if !metadata[key] {
			return false, fmt.Errorf("native task has no attributable container metadata")
		}
	}
	if o.Network == nil || o.Network.OVS == nil {
		return false, fmt.Errorf("scope fabric observer unavailable")
	}
	if err := o.Network.OVS.Ping(ctx); err != nil {
		return false, err
	}
	o.Network.OVS.vethMu.Lock()
	err = o.captureScopeFabric(ctx, id)
	if err == nil {
		err = o.captureScopeAttachments(ctx, id, owners)
	}
	o.Network.OVS.vethMu.Unlock()
	// Even a partial scan can discover an additional positive obligation.
	out.Identity = *id
	if persistErr := o.writeRecord("scope", *id, *id); persistErr != nil {
		return false, persistErr
	}
	if err != nil {
		return false, err
	}
	for _, binding := range id.VNIBindings {
		var object client.Object
		switch binding.Kind {
		case ownerKindDevice:
			object = &lab.Device{}
		case ownerKindConnection:
			object = &lab.Connection{}
		default:
			return false, ErrPortOwnerUnknown
		}
		if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, object); err == nil && string(object.GetUID()) == binding.UID && !object.GetDeletionTimestamp().IsZero() {
			if err := o.Network.OVS.RetireVNIOwned(ctx, binding, o.Network.Flows); err != nil {
				return false, err
			}
		}
	}
	for _, binding := range id.VNIBindings {
		if o.vniReleased(binding) {
			out.ReleasedVNIs = append(out.ReleasedVNIs, binding)
		}
	}
	if err := o.writeRecord("scope", *id, *id); err != nil {
		return false, err
	}
	present, _, err := nativeTaskPresence(list.Tasks, owned)
	if err != nil {
		return false, err
	}
	if present {
		out.RuntimeState = "Allocated"
		return true, nil
	}
	populated, err := o.cgroupsPresent(cgroups)
	if err != nil {
		return false, err
	}
	if populated {
		return false, fmt.Errorf("scope cgroup remains populated")
	}
	out.RuntimeAbsentAt = &now
	out.CgroupAbsentAt = &now
	return false, nil
}

func (o *NativeRuntimeObserver) observeAbsentScope(ctx context.Context, id lab.OwnedRuntimeIdentity, out lab.OwnedRuntimeReport, now metav1.Time, stopped bool, owners []lab.OwnedRuntimeIdentity, retirement *lab.LifecycleRetirementIntent, scopeFence func(lab.OwnedRuntimeIdentity) (bool, error)) lab.OwnedRuntimeReport {
	fail := func(err error) lab.OwnedRuntimeReport { out.Error = err.Error(); return out }
	if o.Network == nil || o.Network.OVS == nil || o.Network.Flows == nil {
		return fail(fmt.Errorf("scope fabric observer unavailable"))
	}
	if err := o.Network.OVS.Ping(ctx); err != nil {
		return fail(err)
	}
	if err := o.captureScopeFabric(ctx, &id); err != nil {
		return fail(err)
	}
	out.Identity = id
	if !stopped {
		if id.ScopeKind == runtimeNeverMaterialized {
			if len(id.PortKeys) > 0 || len(id.FabricPorts) > 0 {
				return fail(fmt.Errorf("never-materialized scope has native attachments"))
			}
			if err := o.Network.Flows.client.Barrier(); err != nil {
				return fail(err)
			}
			id.AttachmentsComplete = true
			out.Identity = id
			out.RuntimeState = "Vacant"
			out.AttachmentsAbsentAt = &now
		} else {
			out.RuntimeState = "Allocated"
		}
		return out
	}
	// Durable declaration/native actual inventory before any fabric retirement.
	if err := o.writeRecord("scope", id, id); err != nil {
		return fail(err)
	}
	for _, port := range id.PortRows {
		if err := o.Network.DelVethWithFlowsExpected(port.Key, types.UID(port.OwnerUID), port.RowUUID); err != nil {
			return fail(err)
		}
	}
	for _, port := range id.FabricPorts {
		if _, err := scopeFence(id); err != nil {
			return fail(err)
		}
		if err := o.Network.OVS.DelFabricPortOwned(port.Key, types.UID(port.OwnerUID), o.Network.Flows, port.RowUUID); err != nil {
			return fail(err)
		}
	}
	o.Network.OVS.vethMu.Lock()
	defer o.Network.OVS.vethMu.Unlock()
	if _, err := scopeFence(id); err != nil {
		return fail(err)
	}
	for _, binding := range id.VNIBindings {
		if err := o.retireVNI(ctx, binding, o.Network.Flows); err != nil {
			return fail(err)
		}
	}
	if err := o.Network.Flows.client.Barrier(); err != nil {
		return fail(err)
	}
	// A fresh native enumeration after the correlated barriers must find none
	// of the positively recorded owned rows or kernel links.
	for _, port := range id.PortRows {
		o.Network.OVS.mu.Lock()
		row, err := o.Network.OVS.portSnapshotLocked(port.Key)
		o.Network.OVS.mu.Unlock()
		if err != nil {
			return fail(err)
		}
		if row != nil {
			return fail(fmt.Errorf("owned attachment remains"))
		}
	}
	if err := o.captureScopeAttachments(ctx, &id, owners, true); err != nil {
		return fail(err)
	}
	id.AttachmentsComplete = true
	out.Identity = id
	// The correlated barrier is the physical flow authority; fsync the exact
	// object/node/operation scope before publishing its release acknowledgement.
	out.ReleasedVNIs = append([]lab.OwnedVNI(nil), id.VNIBindings...)
	out.AttachmentsAbsentAt = &now
	out.RuntimeState = runtimeReleased
	if retirement != nil {
		if err := o.retirementScopeCurrent(ctx, id, *retirement); err != nil {
			return fail(err)
		}
	}
	if err := o.writeRecord("scope-fabric-released", id, out); err != nil {
		return fail(err)
	}
	return out
}

func (o *NativeRuntimeObserver) captureConnectionFabric(id *lab.OwnedRuntimeIdentity, connections []lab.Connection) error {
	for _, connection := range connections {
		owned := false
		for _, parent := range connection.OwnerReferences {
			owned = owned || parent.Kind == ownerKindLab && string(parent.UID) == id.OwnerUID
		}
		if !owned {
			continue
		}

		for _, endpoint := range connection.Spec.Endpoints {
			key := patchPortName(connection.Namespace, connection.Name, endpoint.Device)
			o.Network.OVS.mu.Lock()
			row, err := o.Network.OVS.fabricSnapshotLocked(key)
			o.Network.OVS.mu.Unlock()
			if err != nil {
				return err
			}
			if row == nil {
				var receipt fabricReceipt
				record := lab.OwnedRuntimeIdentity{ScopeUID: string(connection.UID), NodeName: o.NodeName, NodeBootID: o.BootID}
				if o.readRecord("fabric-released-"+key, record, &receipt) == nil && receipt.ConnectionUID == string(connection.UID) && receipt.RowUUID != "" {
					id.FabricPorts = append(id.FabricPorts, lab.OwnedFabricPort{Key: key, OwnerUID: string(connection.UID), RowUUID: receipt.RowUUID})
				}
			}
			if row != nil {
				if row.ExternalIDs[fabricOwnerExternalID] != string(connection.UID) {
					return ErrPortOwnerUnknown
				}
				entry := lab.OwnedFabricPort{Key: key, OwnerUID: string(connection.UID), RowUUID: row.UUID}
				exists := false
				for _, old := range id.FabricPorts {
					exists = exists || reflect.DeepEqual(entry, old)
				}
				if !exists {
					id.FabricPorts = append(id.FabricPorts, entry)
				}
			}
		}
	}
	return nil
}

func (o *NativeRuntimeObserver) uniqueVNI(ctx context.Context, vni uint) error {
	var ds lab.DeviceList
	var cs lab.ConnectionList
	if err := o.Reader.List(ctx, &ds); err != nil {
		return err
	}
	if err := o.Reader.List(ctx, &cs); err != nil {
		return err
	}
	count := 0
	for _, d := range ds.Items {
		if d.Status.VNI != nil && *d.Status.VNI == vni {
			count++
		}
	}
	for _, c := range cs.Items {
		if c.Status.VNI != nil && *c.Status.VNI == vni {
			count++
		}
	}
	if count != 1 {
		return ErrPortOwnerChanged
	}
	return nil
}

func (o *NativeRuntimeObserver) currentScopeRetirement(ctx context.Context, id lab.OwnedRuntimeIdentity, fresh []bool) *lab.LifecycleRetirementIntent {
	var retirement *lab.LifecycleRetirementIntent
	if len(fresh) > 0 && fresh[0] && (id.ScopeKind == scopeLabFabric || id.ScopeKind == runtimeNeverMaterialized) {
		var l lab.Lab
		if o.Reader != nil && o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.LabName}, &l) == nil {
			if in, valid := nativeLabRetirementChallenge(&l, id, l.Status.ScopeInventory); valid {
				retirement = &in
			}
		}
	}
	return retirement
}

func vniObjectOwned(object client.Object, parent *lab.Lab) bool {
	owned := false
	for _, ref := range object.GetOwnerReferences() {
		owned = owned || ref.Kind == ownerKindLab && ref.UID == parent.UID
	}
	return owned
}
