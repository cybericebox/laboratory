package laboratory

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NativeGroupServiceReleaseObserver consumes immutable controller inventory and
// bound node reports. No Pod list or aggregate timestamp can substitute for it.
type NativeGroupServiceReleaseObserver struct{ Client client.Client }

func (o NativeGroupServiceReleaseObserver) GroupServicesReleased(ctx context.Context, g *lab.LabGroup) (bool, error) {
	if g.Spec.Lifecycle == nil || !g.Spec.Lifecycle.IsStopped() || g.Status.Lifecycle == nil || g.Status.Lifecycle.LabUID != string(g.UID) || g.Status.Lifecycle.OperationID != g.Spec.Lifecycle.OperationID || g.Status.Lifecycle.Revision != g.Spec.Lifecycle.Revision || g.Status.Lifecycle.ObservedGeneration != g.Generation {
		return false, nil
	}
	a := aggregateRuntime(g.Status.ServiceRuntime, g.Status.ServiceReports, string(g.UID), g.Status.Lifecycle.OperationID, g.Status.Lifecycle.Revision)
	released := a.RuntimeState == "Released"
	if o.Client != nil && !reflect.DeepEqual(g.Status.Resources, a) {
		base := g.DeepCopy()
		g.Status.Resources = a
		if e := o.Client.Status().Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); e != nil {
			return false, e
		}
	}
	return released, nil
}
func runtimeRowsReleased(rows []lab.OwnedRuntimeIdentity, reports []lab.OwnedRuntimeReport, uid, op string, rev int64) bool {
	if len(rows) == 0 {
		return false
	}
	for _, id := range rows {
		scope := id.ScopeKind == "LabFabric" || id.ScopeKind == "GroupScope"
		if id.OwnerUID != uid || id.OperationID != op || id.Revision != rev || id.NodeBootID == "" || id.NodeName == "" || scope && (id.ScopeUID != uid || id.Generation < 1 || !id.AttachmentsComplete) || !scope && (id.PodUID == "" || len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 || len(id.PortKeys) == 0 && !id.AttachmentsComplete) {
			return false
		}
		found := false
		for _, r := range reports {
			if reflect.DeepEqual(id, r.Identity) && r.Error == "" && r.RuntimeState == "Released" && nonzeroTime(r.ObservedAt) && nonzeroTime(r.RuntimeAbsentAt) && nonzeroTime(r.CgroupAbsentAt) && nonzeroTime(r.AttachmentsAbsentAt) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func nonzeroTime(t *metav1.Time) bool { return t != nil && !t.IsZero() }

func aggregateRuntime(rows []lab.OwnedRuntimeIdentity, reports []lab.OwnedRuntimeReport, uid, op string, rev int64) *lab.RuntimeAllocation {
	now := metav1.Now()
	a := &lab.RuntimeAllocation{RuntimeState: "Unknown", OperationID: op, Revision: rev, ObservedAt: &now, StorageState: "Unknown"}
	// Exact historical ACKs retire only their own obligations, continuously.
	// Multiple operation views of one physical incarnation reserve it once.
	currentRows := make([]lab.OwnedRuntimeIdentity, 0, len(rows))
	held := map[string]lab.ResourceAmounts{}
	configured := map[string]lab.OwnedRuntimeIdentity{}
	for _, id := range rows {
		isCurrent := id.OwnerUID == uid && id.OperationID == op && id.Revision == rev
		resolved := runtimeRowsReleased([]lab.OwnedRuntimeIdentity{id}, reports, id.OwnerUID, id.OperationID, id.Revision)
		if !isCurrent && resolved {
			continue
		}
		currentRows = append(currentRows, id)
		key := id.NodeName + "/" + id.NodeBootID + "/" + id.PodUID
		if id.PodUID == "" {
			key += "/" + id.ScopeUID
		}
		if old, ok := configured[key]; !ok || isCurrent {
			configured[key] = id
		} else {
			_ = old
		}
		if !resolved {
			old := held[key]
			old.CPUMillicores = max(old.CPUMillicores, id.Requests.CPUMillicores)
			old.MemoryBytes = max(old.MemoryBytes, id.Requests.MemoryBytes)
			held[key] = old
		}
	}
	for _, id := range configured {
		a.ConfiguredRequests.CPUMillicores += id.Requests.CPUMillicores
		a.ConfiguredRequests.MemoryBytes += id.Requests.MemoryBytes
		a.ConfiguredLimits.CPUMillicores += id.Limits.CPUMillicores
		a.ConfiguredLimits.MemoryBytes += id.Limits.MemoryBytes
	}
	for _, amount := range held {
		a.AllocatedRequests.CPUMillicores += amount.CPUMillicores
		a.AllocatedRequests.MemoryBytes += amount.MemoryBytes
	}
	if runtimeRowsReleased(currentRows, reports, uid, op, rev) {
		a.RuntimeState = "Released"
		a.AllocatedRequests = lab.ResourceAmounts{}
		a.ReleasedAt = &now
	}
	return a
}
