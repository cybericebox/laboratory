package laboratory

import (
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"time"
)

// GroupServiceReleaseObserver requires exact native runtime/cgroup/attachment
// cleanup acknowledgements for every pre-scale inventory row. Nil is Unknown.
type GroupServiceReleaseObserver interface {
	GroupServicesReleased(context.Context, *lab.LabGroup) (bool, error)
}

func (r *LabGroupReconciler) groupReader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}
func (r *LabGroupReconciler) currentGroup(ctx context.Context, g *lab.LabGroup) (*lab.LabGroup, error) {
	var live lab.LabGroup
	if err := r.groupReader().Get(ctx, client.ObjectKeyFromObject(g), &live); err != nil {
		return nil, err
	}
	if live.UID != g.UID || !live.DeletionTimestamp.IsZero() || !reflect.DeepEqual(live.Spec.Lifecycle, g.Spec.Lifecycle) {
		return nil, fmt.Errorf("group lifecycle identity changed")
	}
	return &live, nil
}

// reconcileGroupLifecycle returns before normal provisioning for stopped intent.
func (r *LabGroupReconciler) reconcileGroupLifecycle(ctx context.Context, g *lab.LabGroup) (bool, ctrl.Result, error) {
	i := g.Spec.Lifecycle
	if i == nil {
		return false, ctrl.Result{}, nil
	}
	live, err := r.currentGroup(ctx, g)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	*g = *live
	now := metav1.Now()
	o := g.Status.Lifecycle
	if o == nil || o.LabUID != string(g.UID) || o.OperationID != i.OperationID || o.Revision != i.Revision || o.ObservedGeneration != g.Generation {
		g.Status.Lifecycle = &lab.LabLifecycleStatus{LabUID: string(g.UID), OperationID: i.OperationID, Revision: i.Revision, ObservedGeneration: g.Generation, ObservedState: "Unknown", RequestedAt: &now}
		if !i.IsStopped() {
			// Services only. Child lifecycle, snapshots and terminal intent are untouched.
			if r.Scheduled {
				g.Status.Pods = nil
				g.Status.Scheduling = nil
			}
		}
		if err := r.Status().Update(ctx, g); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	if !i.IsStopped() {
		return false, ctrl.Result{}, nil
	}
	if !i.RequireAllLabsStopped {
		return true, ctrl.Result{}, r.groupLifecycleStatus(ctx, g, "StopFailed", "AllLabsStoppedRequired")
	}
	if g.Spec.Admission != nil {
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Stopping", "WaitingForChildAdmission")
	}
	all, err := r.allGroupLabsStopped(ctx, g)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !all {
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Stopping", "WaitingForLabs")
	}
	prepared, err := r.PrepareGroupRelease(ctx, g)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !prepared {
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Unknown", "WaitingForNativeInventory")
	}
	// Revalidate all child intents immediately before scaling; stale cached data
	// is never enough. Start/create admission also refuses stopped groups.
	live, err = r.currentGroup(ctx, g)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	*g = *live
	all, err = r.allGroupLabsStopped(ctx, g)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !all {
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Stopping", "WaitingForLabs")
	}
	ns := lab.LabGroupNamespaceOf(g)
	for _, component := range []string{names.ComponentVPN, names.ComponentGateway} {
		if _, err = r.currentGroup(ctx, g); err != nil {
			return true, ctrl.Result{}, err
		}
		var d appsv1.Deployment
		if err = r.groupReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: component}, &d); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return true, ctrl.Result{}, err
		}
		if err := checkServiceGroupUID(&d, string(g.UID)); err != nil {
			return true, ctrl.Result{}, err
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
			owned := false
			for _, row := range g.Status.ServiceRuntime {
				if row.OwnerUID == string(g.UID) && row.OperationID == g.Spec.Lifecycle.OperationID && row.Revision == g.Spec.Lifecycle.Revision && row.Component == component && row.DeploymentUID == string(d.UID) {
					owned = true
					break
				}
			}
			if !owned {
				return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Unknown", "WaitingForNativeInventory")
			}
			d.Spec.Replicas = ptrInt32(0)
			if err = r.Update(ctx, &d); err != nil {
				return true, ctrl.Result{}, err
			}
		}
	}
	// API Pod absence is a necessary additional check; it cannot mint release.
	var pods corev1.PodList
	if err = r.groupReader().List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return true, ctrl.Result{}, err
	}
	for _, p := range pods.Items {
		if serviceComponent(&p) != "" {
			return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Stopping", "WaitingForServicePods")
		}
	}
	released := false
	if r.ServiceReleaseObserver != nil {
		released, err = r.ServiceReleaseObserver.GroupServicesReleased(ctx, g)
		if err != nil {
			return true, ctrl.Result{}, err
		}
	}
	if !released {
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Unknown", "WaitingForNativeServiceRelease")
	}
	return true, ctrl.Result{}, r.groupLifecycleStatus(ctx, g, "Stopped", "ServicesReleased")
}
func (r *LabGroupReconciler) groupLifecycleStatus(ctx context.Context, g *lab.LabGroup, state, reason string) error {
	live, err := r.currentGroup(ctx, g)
	if err != nil {
		return err
	}
	o := live.Status.Lifecycle
	if o == nil {
		return fmt.Errorf("group lifecycle observation missing")
	}
	if o.ObservedState == state && o.Reason == reason && o.ObservedGeneration == live.Generation {
		return nil
	}
	o.ObservedState = state
	o.Reason = reason
	o.Error = ""
	o.ObservedGeneration = live.Generation
	if state == "Stopped" {
		now := metav1.Now()
		o.StoppedAt = &now
		live.Status.VPN.Registered = false
		live.Status.Suspended = true
		live.Status.Phase = lab.PhaseSuspended
	}
	return r.Status().Update(ctx, live)
}
func (r *LabGroupReconciler) allGroupLabsStopped(ctx context.Context, g *lab.LabGroup) (bool, error) {
	var children lab.LabList
	if err := r.groupReader().List(ctx, &children, client.InNamespace(lab.LabGroupNamespaceOf(g))); err != nil {
		return false, err
	}
	for n := range children.Items {
		l := &children.Items[n]
		if !l.DeletionTimestamp.IsZero() || !retirementReady(l) {
			return false, nil
		}
		if l.Status.Scheduling != nil && l.Status.Scheduling.Pending > 0 {
			return false, nil
		}
	}
	// Device queues survive interrupted controllers; do not admit a group stop
	// while any child has a queued/starting runtime admission.
	var devices lab.DeviceList
	if err := r.groupReader().List(ctx, &devices, client.InNamespace(lab.LabGroupNamespaceOf(g))); err != nil {
		return false, err
	}
	for _, d := range devices.Items {
		if d.Status.Scheduling != nil && (d.Status.Scheduling.State == lab.PodQueued || d.Status.Scheduling.State == lab.PodStarting) {
			return false, nil
		}
	}
	return true, nil
}
func serviceComponent(p *corev1.Pod) string {
	if _, device := p.Labels[names.LabelLab]; device {
		return ""
	}
	c := p.Labels[names.LabelComponent]
	if c == "" {
		c = p.Labels[labelApp]
	}
	if c == names.ComponentVPN || c == names.ComponentGateway {
		return c
	}
	return ""
}

// PrepareGroupRelease persists complete native identities before replica changes.
// A missing node handshake prevents scaling, including during recovery.
func (r *LabGroupReconciler) PrepareGroupRelease(ctx context.Context, g *lab.LabGroup) (bool, error) {
	live, err := r.currentGroup(ctx, g)
	if err != nil {
		return false, err
	}
	var pods corev1.PodList
	ns := lab.LabGroupNamespaceOf(live)
	if err = r.groupReader().List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return false, err
	}
	rows := []lab.OwnedRuntimeIdentity{}
	for _, old := range live.Status.ServiceRuntime {
		if old.OperationID == live.Spec.Lifecycle.OperationID && old.Revision == live.Spec.Lifecycle.Revision {
			rows = append(rows, old)
			continue
		}
		released := false
		for _, report := range live.Status.ServiceReports {
			if reflect.DeepEqual(old, report.Identity) && report.RuntimeState == "Released" && report.Error == "" && report.ObservedAt != nil && !report.ObservedAt.IsZero() && report.RuntimeAbsentAt != nil && !report.RuntimeAbsentAt.IsZero() && report.CgroupAbsentAt != nil && !report.CgroupAbsentAt.IsZero() && report.AttachmentsAbsentAt != nil && !report.AttachmentsAbsentAt.IsZero() {
				released = true
				break
			}
		}
		if !released {
			rows = append(rows, old)
		}
	}
	for _, p := range pods.Items {
		component := serviceComponent(&p)
		if component == "" {
			continue
		}
		var d appsv1.Deployment
		if err = r.groupReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: component}, &d); err != nil {
			return false, err
		}
		if !r.servicePodOwned(ctx, &p, &d) {
			return false, fmt.Errorf("foreign service Pod %s", p.Name)
		}
		var node corev1.Node
		if p.Spec.NodeName == "" {
			return false, nil
		}
		if err = r.groupReader().Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node); err != nil {
			return false, err
		}
		ids := []string{}
		for _, c := range p.Status.ContainerStatuses {
			if c.ContainerID != "" {
				ids = append(ids, c.ContainerID)
			}
		}
		if len(ids) == 0 || node.Status.NodeInfo.BootID == "" {
			return false, nil
		}
		var identity *lab.OwnedRuntimeIdentity
		for n := range live.Status.ServiceReports {
			report := &live.Status.ServiceReports[n]
			candidate := &report.Identity
			if candidate.OwnerUID == string(live.UID) && candidate.OperationID == live.Spec.Lifecycle.OperationID && candidate.Revision == live.Spec.Lifecycle.Revision && candidate.DeploymentUID == string(d.UID) && candidate.PodUID == string(p.UID) && candidate.NodeName == p.Spec.NodeName && candidate.NodeBootID == node.Status.NodeInfo.BootID && candidate.Component == component && containsStrings(candidate.ContainerIDs, ids) && len(candidate.CgroupPaths) > 0 && len(candidate.PortKeys) > 0 && report.ObservedAt != nil && report.Error == "" {
				identity = candidate
				break
			}
		}
		if identity == nil {
			return false, nil
		}
		have := false
		for _, row := range rows {
			if reflect.DeepEqual(row, *identity) {
				have = true
				break
			}
		}
		if !have {
			rows = append(rows, *identity.DeepCopy())
		}
	}
	if !reflect.DeepEqual(rows, live.Status.ServiceRuntime) {
		live.Status.ServiceRuntime = rows
		if err = r.Status().Update(ctx, live); err != nil {
			return false, err
		}
	}
	*g = *live
	// A group with no recorded services is still Unknown: absent APIs cannot
	// prove a lost runtime or the original node's cleanup.
	return len(rows) > 0, nil
}
func containsStrings(all, subset []string) bool {
	for _, id := range subset {
		found := false
		for _, native := range all {
			if normalizeContainerID(native) == normalizeContainerID(id) {
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
func normalizeContainerID(id string) string {
	for n := 0; n+2 < len(id); n++ {
		if id[n:n+3] == "://" {
			return id[n+3:]
		}
	}
	return id
}
func (r *LabGroupReconciler) servicePodOwned(ctx context.Context, p *corev1.Pod, d *appsv1.Deployment) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind != "ReplicaSet" || o.Controller == nil || !*o.Controller {
			continue
		}
		var rs appsv1.ReplicaSet
		if err := r.groupReader().Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: o.Name}, &rs); err != nil || rs.UID != o.UID {
			continue
		}
		for _, owner := range rs.OwnerReferences {
			if owner.Kind == "Deployment" && owner.UID == d.UID && owner.Controller != nil && *owner.Controller {
				return true
			}
		}
	}
	return false
}
func (r *LabGroupReconciler) observeGroupStart(ctx context.Context, g *lab.LabGroup) error {
	i := g.Spec.Lifecycle
	if i == nil || i.IsStopped() {
		return nil
	}
	ns := lab.LabGroupNamespaceOf(g)
	for _, component := range groupPodNames(g) {
		var d appsv1.Deployment
		if err := r.groupReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: component}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				return r.groupLifecycleStatus(ctx, g, "Starting", "WaitingForServices")
			}
			return err
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Status.ObservedGeneration != d.Generation || d.Status.ReadyReplicas < 1 {
			return r.groupLifecycleStatus(ctx, g, "Starting", "WaitingForServices")
		}
	}
	return r.groupLifecycleStatus(ctx, g, "Running", "ServicesReady")
}
