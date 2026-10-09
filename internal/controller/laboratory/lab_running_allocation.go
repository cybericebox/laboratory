package laboratory

import (
	"context"
	"reflect"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Publish current allocation only from native presence bound to the actual
// objects. API absence and an earlier operation's release never mint credit.
func (r *LabReconciler) runningLabAllocation(ctx context.Context, l *lab.Lab, devices []lab.Device) (*lab.RuntimeAllocation, error) {
	i := l.Spec.Lifecycle
	a := &lab.RuntimeAllocation{RuntimeState: observationStateUnknown, OperationID: i.OperationID, Revision: i.Revision, StorageState: observationStateUnknown}
	o := &runningAllocationObservation{lab: l, allocation: a, known: r.RuntimeObservation && labStartPrepared(l), held: map[string]lab.ResourceAmounts{}, byName: map[string]*lab.Device{}, materialized: map[string]bool{}, present: map[string]lab.OwnedRuntimeIdentity{}, declared: map[string]bool{}}
	var pods corev1.PodList
	if err := r.lifecycleReader().List(ctx, &pods, client.InNamespace(l.Namespace)); err != nil {
		return nil, err
	}
	for _, template := range l.Spec.Devices {
		o.declared[template.Name] = true
	}
	if err := r.observeRunningDeviceHoldings(ctx, o, devices, pods); err != nil {
		return nil, err
	}
	if err := r.observeConfiguredRunningAllocation(ctx, o, pods); err != nil {
		return nil, err
	}
	verifyRunningAllocationHistory(o, devices)
	if err := r.observeRunningFabricAllocation(ctx, o); err != nil {
		return nil, err
	}
	for _, amount := range o.held {
		o.allocation.AllocatedRequests = allocationSum(o.allocation.AllocatedRequests, amount)
	}
	if o.known {
		o.allocation.RuntimeState = runtimeStateAllocated
	} else if l.Status.Resources != nil {
		// Lost inventories cannot erase an already published positive hold.
		o.allocation.AllocatedRequests = allocationMaximum(o.allocation.AllocatedRequests, l.Status.Resources.AllocatedRequests)
	}
	if l.Status.Resources != nil {
		o.allocation.SnapshotQuotaBytes = max(o.allocation.SnapshotQuotaBytes, l.Status.Resources.SnapshotQuotaBytes)
	}
	return o.allocation, nil
}

func currentAllocationIdentity(l *lab.Lab, id lab.OwnedRuntimeIdentity) bool {
	return id.OwnerUID == string(l.UID) && id.Namespace == l.Namespace && id.LabName == l.Name && id.OperationID == l.Spec.Lifecycle.OperationID && id.Revision == l.Spec.Lifecycle.Revision && id.Generation == l.Generation
}
func deviceAllocationState(d *lab.Device, id lab.OwnedRuntimeIdentity) bool {
	if d.Status.State == nil {
		// Ordinary Deployment reports have no persistence state tuple.
		return !deviceStateEnabled(d) && id.Epoch == 0 && id.Incarnation == 0
	}
	return id.Epoch == d.Status.State.Epoch && id.Incarnation == d.Status.State.Incarnation && id.Incarnation > 0
}
func (r *LabReconciler) publishUnknownRunningAllocation(ctx context.Context, l *lab.Lab) error {
	var devices lab.DeviceList
	if err := r.lifecycleReader().List(ctx, &devices, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	a, err := r.runningLabAllocation(ctx, l, devices.Items)
	if err != nil {
		return err
	}
	a.RuntimeState = observationStateUnknown
	if reflect.DeepEqual(a, l.Status.Resources) {
		return nil
	}
	base := l.DeepCopy()
	l.Status.Resources = a
	return r.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
func freshAllocationTime(t *metav1.Time) bool {
	return nonzeroTime(t) && time.Since(t.Time) >= 0 && time.Since(t.Time) <= 60*time.Second
}
func allocationRowReleased(row lab.OwnedRuntimeIdentity, reports []lab.OwnedRuntimeReport) bool {
	// Immutable native death certificates retire only their exact obligation.
	// Their durability is independent of the TTL for current live presence.
	return runtimeRowsReleased([]lab.OwnedRuntimeIdentity{row}, reports, row.OwnerUID, row.OperationID, row.Revision)
}
func emptyAllocationDeclaration(id lab.OwnedRuntimeIdentity) bool {
	return id.PodUID == "" && len(id.ContainerIDs) == 0 && len(id.CgroupPaths) == 0 && len(id.PortKeys) == 0 && len(id.PortRows) == 0 && len(id.FabricPorts) == 0 && len(id.VNIs) == 0 && len(id.VNIBindings) == 0 && id.Requests == (lab.ResourceAmounts{}) && id.Limits == (lab.ResourceAmounts{})
}
func allocationScopeContains(prior, sample lab.OwnedRuntimeIdentity) bool {
	return sameDeclaredScope(prior, sample) && scopeHistoryContains(sample.ContainerIDs, prior.ContainerIDs) && scopeHistoryContains(sample.CgroupPaths, prior.CgroupPaths) && scopeHistoryContains(sample.PortKeys, prior.PortKeys) && scopeHistoryContains(sample.PortRows, prior.PortRows) && scopeHistoryContains(sample.FabricPorts, prior.FabricPorts) && scopeHistoryContains(sample.VNIs, prior.VNIs) && scopeHistoryContains(sample.VNIBindings, prior.VNIBindings)
}
func allocationPhysicalKey(id lab.OwnedRuntimeIdentity) string {
	key := id.NodeName + "/" + id.NodeBootID + "/" + id.PodUID
	if id.PodUID == "" {
		key += "/" + id.ScopeUID
	}
	return key
}
func allocationMaximum(a, b lab.ResourceAmounts) lab.ResourceAmounts {
	return lab.ResourceAmounts{CPUMillicores: max(a.CPUMillicores, b.CPUMillicores), MemoryBytes: max(a.MemoryBytes, b.MemoryBytes)}
}
func allocationSum(a, b lab.ResourceAmounts) lab.ResourceAmounts {
	return lab.ResourceAmounts{CPUMillicores: a.CPUMillicores + b.CPUMillicores, MemoryBytes: a.MemoryBytes + b.MemoryBytes}
}
func allocationDifference(a, b lab.ResourceAmounts) lab.ResourceAmounts {
	return lab.ResourceAmounts{CPUMillicores: max(int64(0), a.CPUMillicores-b.CPUMillicores), MemoryBytes: max(int64(0), a.MemoryBytes-b.MemoryBytes)}
}
func allocationAmounts(list corev1.ResourceList) lab.ResourceAmounts {
	return lab.ResourceAmounts{CPUMillicores: list.Cpu().MilliValue(), MemoryBytes: list.Memory().Value()}
}
func allocationPodResources(containers []corev1.Container) (request, limit lab.ResourceAmounts) {
	for _, c := range containers {
		request = allocationSum(request, allocationAmounts(c.Resources.Requests))
		limit = allocationSum(limit, allocationAmounts(c.Resources.Limits))
	}
	return
}
func allocationOwnedPodResources(p *corev1.Pod) (request, limit lab.ResourceAmounts) {
	request, limit = allocationPodResources(p.Spec.Containers)
	// Device init containers are sequential; scheduling holds their maximum,
	// even when the native main-container report declares a smaller amount.
	for _, init := range p.Spec.InitContainers {
		request = allocationMaximum(request, allocationAmounts(init.Resources.Requests))
		limit = allocationMaximum(limit, allocationAmounts(init.Resources.Limits))
	}
	overhead := allocationAmounts(p.Spec.Overhead)
	return allocationSum(request, overhead), allocationSum(limit, overhead)
}
func podContainerIDs(p *corev1.Pod) []string {
	var ids []string
	for _, status := range append(append([]corev1.ContainerStatus(nil), p.Status.ContainerStatuses...), p.Status.InitContainerStatuses...) {
		if status.ContainerID != "" {
			ids = append(ids, status.ContainerID)
		}
	}
	return ids
}
func runningAllocationRequeue(l *lab.Lab) ctrl.Result {
	if l.Spec.Lifecycle != nil && l.Spec.Lifecycle.DesiredState == lifecycleStateRunning {
		return ctrl.Result{RequeueAfter: 30 * time.Second}
	}
	return ctrl.Result{}
}

// runningAllocationObservation retains every physical hold while collecting
// current native presence. All phases share the same conservative ledger.
type runningAllocationObservation struct {
	lab          *lab.Lab
	allocation   *lab.RuntimeAllocation
	known        bool
	held         map[string]lab.ResourceAmounts
	byName       map[string]*lab.Device
	materialized map[string]bool
	present      map[string]lab.OwnedRuntimeIdentity
	declared     map[string]bool
}

func (o *runningAllocationObservation) add(key string, amount lab.ResourceAmounts) {
	o.held[key] = allocationMaximum(o.held[key], amount)
}
func (o *runningAllocationObservation) observe(t *metav1.Time) {
	if nonzeroTime(t) && (o.allocation.ObservedAt == nil || t.Before(o.allocation.ObservedAt)) {
		o.allocation.ObservedAt = t.DeepCopy()
	}
}
func (r *LabReconciler) observeRunningDeviceHoldings(ctx context.Context, o *runningAllocationObservation, devices []lab.Device, pods corev1.PodList) error {
	l := o.lab
	for n := range devices {
		d := &devices[n]
		if !ownedLabDevice(l, d) {
			continue
		}
		o.byName[d.Spec.Name] = d
		if d.Status.State != nil {
			o.allocation.SnapshotQuotaBytes += d.Status.State.SizeBytes
		}
		rows := append(append([]lab.OwnedRuntimeIdentity(nil), d.Status.RuntimeInventory...), reportHistory(d.Status.RuntimeReports)...)
		for _, row := range rows {
			if !devicePlacementIdentity(l, d, row) || allocationRowReleased(row, d.Status.RuntimeReports) {
				continue
			}
			o.add(allocationPhysicalKey(row), row.Requests)
		}
		if d.Spec.Type == lab.DeviceTypeContainer && !o.declared[d.Spec.Name] {
			// Pruning an API object is not proof that its last Pod stopped.
			o.known = false
			key := "undeclared/" + string(d.UID)
			for _, row := range rows {
				if devicePlacementIdentity(l, d, row) && !allocationRowReleased(row, d.Status.RuntimeReports) {
					key = allocationPhysicalKey(row)
				}
			}
			for n := range pods.Items {
				p := &pods.Items[n]
				owned, err := r.ownedRuntimeDevicePod(ctx, d, p)
				if err != nil {
					return err
				}
				if owned {
					var node corev1.Node
					if p.Spec.NodeName != "" {
						if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node); err != nil && !apierrors.IsNotFound(err) {
							return err
						}
					}
					key = allocationPhysicalKey(lab.OwnedRuntimeIdentity{NodeName: p.Spec.NodeName, NodeBootID: node.Status.NodeInfo.BootID, PodUID: string(p.UID)})
					amount, _ := allocationOwnedPodResources(p)
					o.add(key, amount)
				}
			}
			o.add(key, allocationAmounts(deviceResources(d, r.Defaults).Requests))
		}
	}
	return nil
}
func runningDeviceCurrentKey(l *lab.Lab, d *lab.Device, id lab.OwnedRuntimeIdentity) bool {
	return currentAllocationIdentity(l, id) && id.ScopeUID == string(d.UID) && id.PodUID != "" && id.NodeName == d.Status.NodeName && deviceAllocationState(d, id)
}
func runningDeviceReportMissing(l *lab.Lab, d *lab.Device, p *corev1.Pod, node *corev1.Node, report lab.OwnedRuntimeReport) bool {
	id := report.Identity
	return !currentAllocationIdentity(l, id) || id.ScopeUID != string(d.UID) || id.PodUID != string(p.UID) || id.NodeName != p.Spec.NodeName || id.NodeBootID == "" || id.NodeBootID != node.Status.NodeInfo.BootID || !d.Status.Ready || !deviceAllocationState(d, id) || len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 || !id.AttachmentsComplete || !containsStrings(id.ContainerIDs, podContainerIDs(p)) || report.Error != "" || report.RuntimeState != runtimeStateAllocated || !freshAllocationTime(report.ObservedAt) || !p.DeletionTimestamp.IsZero()
}
func (r *LabReconciler) observeConfiguredRunningAllocation(ctx context.Context, o *runningAllocationObservation, pods corev1.PodList) error {
	l := o.lab
	for _, template := range l.Spec.Devices {
		if template.Type != lab.DeviceTypeContainer {
			continue
		}
		configured := deviceResources(&lab.Device{Spec: lab.DeviceSpec{Resources: template.Resources}}, r.Defaults)
		request, limit := allocationPodResources([]corev1.Container{{Resources: configured}})
		o.allocation.ConfiguredRequests = allocationSum(o.allocation.ConfiguredRequests, request)
		o.allocation.ConfiguredLimits = allocationSum(o.allocation.ConfiguredLimits, limit)
		d := o.byName[template.Name]
		currentKey := "configured/" + template.Name
		deviceKnown := false
		if d != nil {
			actual := deviceResources(d, r.Defaults)
			request = allocationMaximum(request, allocationAmounts(actual.Requests))
			limit = allocationMaximum(limit, allocationAmounts(actual.Limits))
			// An API-lost current Pod still names one physical obligation, rather
			// than a second configured placeholder. It cannot certify presence.
			for _, report := range d.Status.RuntimeReports {
				id := report.Identity
				if runningDeviceCurrentKey(l, d, id) {
					currentKey = allocationPhysicalKey(id)
				}
			}
			for n := range pods.Items {
				p := &pods.Items[n]
				owned, err := r.ownedRuntimeDevicePod(ctx, d, p)
				if err != nil {
					return err
				}
				if !owned {
					continue
				}
				var node corev1.Node
				if p.Spec.NodeName != "" {
					err = r.lifecycleReader().Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node)
				}
				if err != nil && !apierrors.IsNotFound(err) && p.Spec.NodeName != "" {
					return err
				}
				key := allocationPhysicalKey(lab.OwnedRuntimeIdentity{NodeName: p.Spec.NodeName, NodeBootID: node.Status.NodeInfo.BootID, PodUID: string(p.UID)})
				podRequest, podLimit := allocationOwnedPodResources(p)
				o.add(key, podRequest)
				if p.Name != d.Status.PodName || p.Spec.NodeName != d.Status.NodeName {
					o.known = false
					continue
				}
				currentKey = key
				request = allocationMaximum(request, podRequest)
				limit = allocationMaximum(limit, podLimit)
				for _, report := range d.Status.RuntimeReports {
					id := report.Identity
					if runningDeviceReportMissing(l, d, p, &node, report) {
						continue
					}
					deviceKnown = true
					o.materialized[string(d.UID)] = true
					o.present[key] = id
					request = allocationMaximum(request, id.Requests)
					limit = allocationMaximum(limit, id.Limits)
					o.observe(report.ObservedAt)
				}
			}
		}
		o.add(currentKey, request)
		o.known = o.known && deviceKnown
		// Actual Pods/native reports can exceed today's clamped configuration.
		o.allocation.ConfiguredRequests = allocationSum(o.allocation.ConfiguredRequests, allocationDifference(request, allocationAmounts(configured.Requests)))
		o.allocation.ConfiguredLimits = allocationSum(o.allocation.ConfiguredLimits, allocationDifference(limit, allocationAmounts(configured.Limits)))
	}
	return nil
}
func verifyRunningAllocationHistory(o *runningAllocationObservation, devices []lab.Device) {
	l := o.lab
	for _, d := range devices {
		if !ownedLabDevice(l, &d) {
			continue
		}
		for _, row := range append(append([]lab.OwnedRuntimeIdentity(nil), d.Status.RuntimeInventory...), reportHistory(d.Status.RuntimeReports)...) {
			if !devicePlacementIdentity(l, &d, row) || allocationRowReleased(row, d.Status.RuntimeReports) {
				continue
			}
			current, currentPresent := o.present[allocationPhysicalKey(row)]
			if !currentAllocationIdentity(l, row) || !o.materialized[string(d.UID)] || !currentPresent || row.PodUID == "" || !deviceAllocationState(&d, row) || !scopeHistoryContains(current.ContainerIDs, row.ContainerIDs) || !scopeHistoryContains(current.CgroupPaths, row.CgroupPaths) || !scopeHistoryContains(current.PortKeys, row.PortKeys) || !scopeHistoryContains(current.PortRows, row.PortRows) {
				o.known = false
			}
		}
	}
}
func (r *LabReconciler) observeRunningFabricAllocation(ctx context.Context, o *runningAllocationObservation) error {
	l := o.lab
	// Every declared placement node needs its own fresh whole-fabric sample.
	scopes := adoptReleasedScopeHistory(l.Status.ScopeInventory, l.Status.ScopeReports)
	haveFabric := false
	for _, scope := range scopes {
		if scope.OwnerUID != string(l.UID) || scope.Namespace != l.Namespace || scope.LabName != l.Name {
			o.known = false
			continue
		}
		if scope.ScopeKind == scopeKindNeverMaterialized && o.materialized[scope.ScopeUID] && emptyAllocationDeclaration(scope) {
			continue
		}
		if !currentAllocationIdentity(l, scope) {
			if !allocationRowReleased(scope, l.Status.ScopeReports) {
				o.known = false
				o.add(allocationPhysicalKey(scope), scope.Requests)
			}
			continue
		}
		if scope.ScopeKind != scopeKindLabFabric || scope.ScopeUID != string(l.UID) {
			o.known = false
			o.add(allocationPhysicalKey(scope), scope.Requests)
			continue
		}
		haveFabric = true
		var node corev1.Node
		err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: scope.NodeName}, &node)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		found := false
		for _, report := range l.Status.ScopeReports {
			// Running whole-scope presence does not set AttachmentsComplete:
			// that flag certifies absence/retirement, not live fabric presence.
			if allocationScopeContains(scope, report.Identity) && scope.NodeBootID != "" && scope.NodeBootID == node.Status.NodeInfo.BootID && report.Error == "" && report.RuntimeState == runtimeStateAllocated && freshAllocationTime(report.ObservedAt) {
				found = true
				o.observe(report.ObservedAt)
			}
		}
		o.known = o.known && found
	}
	o.known = o.known && haveFabric
	return nil
}
