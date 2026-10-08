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
		if id.OwnerUID != uid || id.OperationID != op || id.Revision != rev || id.NodeBootID == "" || id.PodUID == "" || id.NodeName == "" || len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 || len(id.PortKeys) == 0 {
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
	for _, id := range rows {
		a.ConfiguredRequests.CPUMillicores += id.Requests.CPUMillicores
		a.ConfiguredRequests.MemoryBytes += id.Requests.MemoryBytes
		a.ConfiguredLimits.CPUMillicores += id.Limits.CPUMillicores
		a.ConfiguredLimits.MemoryBytes += id.Limits.MemoryBytes
	}
	a.AllocatedRequests = a.ConfiguredRequests
	if runtimeRowsReleased(rows, reports, uid, op, rev) {
		a.RuntimeState = "Released"
		a.AllocatedRequests = lab.ResourceAmounts{}
		a.ReleasedAt = &now
	}
	return a
}
