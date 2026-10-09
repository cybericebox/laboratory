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
	a := &lab.RuntimeAllocation{RuntimeState: "Unknown", OperationID: i.OperationID, Revision: i.Revision, StorageState: "Unknown"}
	known := r.RuntimeObservation && labStartPrepared(l)
	held := map[string]lab.ResourceAmounts{}
	add := func(key string, amount lab.ResourceAmounts) {
		held[key] = allocationMaximum(held[key], amount)
	}
	observe := func(t *metav1.Time) {
		if nonzeroTime(t) && (a.ObservedAt == nil || t.Before(a.ObservedAt)) {
			a.ObservedAt = t.DeepCopy()
		}
	}
	var pods corev1.PodList
	if err := r.lifecycleReader().List(ctx, &pods, client.InNamespace(l.Namespace)); err != nil {
		return nil, err
	}
	byName := map[string]*lab.Device{}
	materialized := map[string]bool{}
	present := map[string]lab.OwnedRuntimeIdentity{}
	declared := map[string]bool{}
	for _, template := range l.Spec.Devices {
		declared[template.Name] = true
	}
	for n := range devices {
		d := &devices[n]
		if !ownedLabDevice(l, d) {
			continue
		}
		byName[d.Spec.Name] = d
		if d.Status.State != nil {
			a.SnapshotQuotaBytes += d.Status.State.SizeBytes
		}
		rows := append(append([]lab.OwnedRuntimeIdentity(nil), d.Status.RuntimeInventory...), reportHistory(d.Status.RuntimeReports)...)
		for _, row := range rows {
			if !devicePlacementIdentity(l, d, row) || allocationRowReleased(row, d.Status.RuntimeReports) {
				continue
			}
			add(allocationPhysicalKey(row), row.Requests)
		}
		if d.Spec.Type == lab.DeviceTypeContainer && !declared[d.Spec.Name] {
			// Pruning an API object is not proof that its last Pod stopped.
			known = false
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
					return nil, err
				}
				if owned {
					var node corev1.Node
					if p.Spec.NodeName != "" {
						if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node); err != nil && !apierrors.IsNotFound(err) {
							return nil, err
						}
					}
					key = allocationPhysicalKey(lab.OwnedRuntimeIdentity{NodeName: p.Spec.NodeName, NodeBootID: node.Status.NodeInfo.BootID, PodUID: string(p.UID)})
					amount, _ := allocationOwnedPodResources(p)
					add(key, amount)
				}
			}
			add(key, allocationAmounts(deviceResources(d, r.Defaults).Requests))
		}
	}
	for _, template := range l.Spec.Devices {
		if template.Type != lab.DeviceTypeContainer {
			continue
		}
		configured := deviceResources(&lab.Device{Spec: lab.DeviceSpec{Resources: template.Resources}}, r.Defaults)
		request, limit := allocationPodResources([]corev1.Container{{Resources: configured}})
		a.ConfiguredRequests = allocationSum(a.ConfiguredRequests, request)
		a.ConfiguredLimits = allocationSum(a.ConfiguredLimits, limit)
		d := byName[template.Name]
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
				if currentAllocationIdentity(l, id) && id.ScopeUID == string(d.UID) && id.PodUID != "" && id.NodeName == d.Status.NodeName && deviceAllocationState(d, id) {
					currentKey = allocationPhysicalKey(id)
				}
			}
			for n := range pods.Items {
				p := &pods.Items[n]
				owned, err := r.ownedRuntimeDevicePod(ctx, d, p)
				if err != nil {
					return nil, err
				}
				if !owned {
					continue
				}
				var node corev1.Node
				if p.Spec.NodeName != "" {
					err = r.lifecycleReader().Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node)
				}
				if err != nil && !apierrors.IsNotFound(err) && p.Spec.NodeName != "" {
					return nil, err
				}
				key := allocationPhysicalKey(lab.OwnedRuntimeIdentity{NodeName: p.Spec.NodeName, NodeBootID: node.Status.NodeInfo.BootID, PodUID: string(p.UID)})
				podRequest, podLimit := allocationOwnedPodResources(p)
				add(key, podRequest)
				if p.Name != d.Status.PodName || p.Spec.NodeName != d.Status.NodeName {
					known = false
					continue
				}
				currentKey = key
				request = allocationMaximum(request, podRequest)
				limit = allocationMaximum(limit, podLimit)
				for _, report := range d.Status.RuntimeReports {
					id := report.Identity
					if !currentAllocationIdentity(l, id) || id.ScopeUID != string(d.UID) || id.PodUID != string(p.UID) || id.NodeName != p.Spec.NodeName || id.NodeBootID == "" || id.NodeBootID != node.Status.NodeInfo.BootID || !d.Status.Ready || !deviceAllocationState(d, id) || len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 || !id.AttachmentsComplete || !containsStrings(id.ContainerIDs, podContainerIDs(p)) || report.Error != "" || report.RuntimeState != "Allocated" || !freshAllocationTime(report.ObservedAt) || !p.DeletionTimestamp.IsZero() {
						continue
					}
					deviceKnown = true
					materialized[string(d.UID)] = true
					present[key] = id
					request = allocationMaximum(request, id.Requests)
					limit = allocationMaximum(limit, id.Limits)
					observe(report.ObservedAt)
				}
			}
		}
		add(currentKey, request)
		known = known && deviceKnown
		// Actual Pods/native reports can exceed today's clamped configuration.
		a.ConfiguredRequests = allocationSum(a.ConfiguredRequests, allocationDifference(request, allocationAmounts(configured.Requests)))
		a.ConfiguredLimits = allocationSum(a.ConfiguredLimits, allocationDifference(limit, allocationAmounts(configured.Limits)))
	}
	for _, d := range devices {
		if !ownedLabDevice(l, &d) {
			continue
		}
		for _, row := range append(append([]lab.OwnedRuntimeIdentity(nil), d.Status.RuntimeInventory...), reportHistory(d.Status.RuntimeReports)...) {
			if !devicePlacementIdentity(l, &d, row) || allocationRowReleased(row, d.Status.RuntimeReports) {
				continue
			}
			current, currentPresent := present[allocationPhysicalKey(row)]
			if !currentAllocationIdentity(l, row) || !materialized[string(d.UID)] || !currentPresent || row.PodUID == "" || !deviceAllocationState(&d, row) || !scopeHistoryContains(current.ContainerIDs, row.ContainerIDs) || !scopeHistoryContains(current.CgroupPaths, row.CgroupPaths) || !scopeHistoryContains(current.PortKeys, row.PortKeys) || !scopeHistoryContains(current.PortRows, row.PortRows) {
				known = false
			}
		}
	}
	// Every declared placement node needs its own fresh whole-fabric sample.
	scopes := adoptReleasedScopeHistory(l.Status.ScopeInventory, l.Status.ScopeReports)
	haveFabric := false
	for _, scope := range scopes {
		if scope.OwnerUID != string(l.UID) || scope.Namespace != l.Namespace || scope.LabName != l.Name {
			known = false
			continue
		}
		if scope.ScopeKind == "NeverMaterialized" && materialized[scope.ScopeUID] && emptyAllocationDeclaration(scope) {
			continue
		}
		if !currentAllocationIdentity(l, scope) {
			if !allocationRowReleased(scope, l.Status.ScopeReports) {
				known = false
				add(allocationPhysicalKey(scope), scope.Requests)
			}
			continue
		}
		if scope.ScopeKind != "LabFabric" || scope.ScopeUID != string(l.UID) {
			known = false
			add(allocationPhysicalKey(scope), scope.Requests)
			continue
		}
		haveFabric = true
		var node corev1.Node
		err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: scope.NodeName}, &node)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		found := false
		for _, report := range l.Status.ScopeReports {
			// Running whole-scope presence does not set AttachmentsComplete:
			// that flag certifies absence/retirement, not live fabric presence.
			if allocationScopeContains(scope, report.Identity) && scope.NodeBootID != "" && scope.NodeBootID == node.Status.NodeInfo.BootID && report.Error == "" && report.RuntimeState == "Allocated" && freshAllocationTime(report.ObservedAt) {
				found = true
				observe(report.ObservedAt)
			}
		}
		known = known && found
	}
	known = known && haveFabric
	for _, amount := range held {
		a.AllocatedRequests = allocationSum(a.AllocatedRequests, amount)
	}
	if known {
		a.RuntimeState = "Allocated"
	} else if l.Status.Resources != nil {
		// Lost inventories cannot erase an already published positive hold.
		a.AllocatedRequests = allocationMaximum(a.AllocatedRequests, l.Status.Resources.AllocatedRequests)
	}
	if l.Status.Resources != nil {
		a.SnapshotQuotaBytes = max(a.SnapshotQuotaBytes, l.Status.Resources.SnapshotQuotaBytes)
	}
	return a, nil
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
	a.RuntimeState = "Unknown"
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
	if l.Spec.Lifecycle != nil && l.Spec.Lifecycle.DesiredState == "Running" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}
	}
	return ctrl.Result{}
}
