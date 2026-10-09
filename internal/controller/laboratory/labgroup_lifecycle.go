package laboratory

import (
	"context"
	"fmt"
	"reflect"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		g.Status.Lifecycle = &lab.LabLifecycleStatus{LabUID: string(g.UID), OperationID: i.OperationID, Revision: i.Revision, ObservedGeneration: g.Generation, ObservedState: observationStateUnknown, RequestedAt: &now}
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
		return r.reconcileGroupLifecycleStart(ctx, g)
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
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, observationStateUnknown, "WaitingForNativeInventory")
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
	if handled, result, err := r.scaleStoppedGroupServices(ctx, g); handled {
		return handled, result, err
	}
	ns := lab.LabGroupNamespaceOf(g)
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
		return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, observationStateUnknown, "WaitingForNativeServiceRelease")
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

// ValidateGroupStop checks the current exact child release certificates before
// the RPC accepts intent. Reconciliation repeats it immediately before scaling.
func (r *LabGroupReconciler) ValidateGroupStop(ctx context.Context, g *lab.LabGroup) error {
	if g.Spec.Admission != nil {
		return fmt.Errorf("group has a pending child admission")
	}
	stopped, err := r.allGroupLabsStopped(ctx, g)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("all child labs must be exactly Stopped and Released with no pending admission")
	}
	return nil
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
	rows := priorGroupReleaseRows(live)
	if r.ServiceReleaseObserver != nil {
		placement, err := r.groupNativePlacement(ctx, live, pods.Items)
		if err != nil {
			return false, err
		}
		scopes, err := declaredScopes(ctx, r.groupReader(), string(live.UID), ns, "", live.Spec.Lifecycle.OperationID, live.Spec.Lifecycle.Revision, live.Generation, "GroupScope", placement)
		if err != nil {
			return false, err
		}
		for _, scope := range scopes {
			for _, report := range live.Status.ServiceReports {
				if sameDeclaredScope(scope, report.Identity) && report.Identity.AttachmentsComplete {
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
	}
	rows, complete, err := r.inventoryGroupServicePods(ctx, live, pods, rows)
	if err != nil || !complete {
		return false, err
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
	return r.groupLifecycleStatus(ctx, g, lifecycleStateRunning, "ServicesReady")
}

func (r *LabGroupReconciler) observeGroupAllocation(ctx context.Context, g *lab.LabGroup) error {
	i := g.Spec.Lifecycle
	if i == nil || i.IsStopped() {
		return nil
	}
	current := aggregateRuntime(g.Status.ServiceRuntime, g.Status.ServiceReports, string(g.UID), i.OperationID, i.Revision)
	configured := lab.ResourceAmounts{}
	known := true
	limits := lab.ResourceAmounts{}
	for _, component := range groupPodNames(g) {
		var deployment appsv1.Deployment
		if err := r.groupReader().Get(ctx, client.ObjectKey{Namespace: lab.LabGroupNamespaceOf(g), Name: component}, &deployment); err != nil {
			if apierrors.IsNotFound(err) {
				known = false
				continue
			}
			return err
		}
		if err := checkServiceGroupUID(&deployment, string(g.UID)); err != nil {
			return err
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			configured.CPUMillicores += container.Resources.Requests.Cpu().MilliValue()
			configured.MemoryBytes += container.Resources.Requests.Memory().Value()
			limits.CPUMillicores += container.Resources.Limits.Cpu().MilliValue()
			limits.MemoryBytes += container.Resources.Limits.Memory().Value()
		}
	}
	current.ConfiguredRequests = configured
	current.ConfiguredLimits = limits
	current.AllocatedRequests.CPUMillicores = max(current.AllocatedRequests.CPUMillicores, configured.CPUMillicores)
	current.AllocatedRequests.MemoryBytes = max(current.AllocatedRequests.MemoryBytes, configured.MemoryBytes)
	current.RuntimeState = runtimeStateAllocated
	if !known {
		current.RuntimeState = observationStateUnknown
		if g.Status.Resources != nil {
			current.AllocatedRequests.CPUMillicores = max(current.AllocatedRequests.CPUMillicores, g.Status.Resources.AllocatedRequests.CPUMillicores)
			current.AllocatedRequests.MemoryBytes = max(current.AllocatedRequests.MemoryBytes, g.Status.Resources.AllocatedRequests.MemoryBytes)
		}
	}
	current.ReleasedAt = nil
	if reflect.DeepEqual(current, g.Status.Resources) {
		return nil
	}
	base := g.DeepCopy()
	g.Status.Resources = current
	return r.Status().Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// Logical Start may supersede stop after inventory but before scale-down. Finish
// only those exact original service obligations; current/replacement services
// and every child intent are outside this drain authority.
func (r *LabGroupReconciler) drainPriorGroupServices(ctx context.Context, g *lab.LabGroup) error {
	intent := g.Spec.Lifecycle
	if intent == nil || intent.IsStopped() {
		return fmt.Errorf("current group start unavailable")
	}
	byDeployment := map[string][]lab.OwnedRuntimeIdentity{}
	for _, row := range g.Status.ServiceRuntime {
		if skipPriorGroupServiceDebt(g, row) {
			continue
		}
		byDeployment[row.Component] = append(byDeployment[row.Component], row)
	}
	ns := lab.LabGroupNamespaceOf(g)
	for component, rows := range byDeployment {
		if component != names.ComponentVPN && component != names.ComponentGateway {
			return fmt.Errorf("prior service component unknown")
		}
		if _, err := r.currentGroup(ctx, g); err != nil {
			return err
		}
		var dep appsv1.Deployment
		if err := r.groupReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: component}, &dep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		for _, row := range rows {
			if string(dep.UID) != row.DeploymentUID {
				return fmt.Errorf("prior service Deployment was replaced")
			}
		}
		if err := checkServiceGroupUID(&dep, string(g.UID)); err != nil {
			return err
		}
		var pods corev1.PodList
		if err := r.groupReader().List(ctx, &pods, client.InNamespace(ns)); err != nil {
			return err
		}
		for n := range pods.Items {
			pod := &pods.Items[n]
			if !r.servicePodOwned(ctx, pod, &dep) {
				continue
			}
			known := groupServicePodRecorded(pod, rows)
			if !known {
				return fmt.Errorf("prior service Pod was replaced")
			}
		}
		if _, err := r.currentGroup(ctx, g); err != nil {
			return err
		}
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
			base := dep.DeepCopy()
			dep.Spec.Replicas = ptrInt32(0)
			if err := r.Patch(ctx, &dep, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		for n := range pods.Items {
			pod := &pods.Items[n]
			if !r.servicePodOwned(ctx, pod, &dep) {
				continue
			}
			known := groupServicePodRecorded(pod, rows)
			if !known {
				return fmt.Errorf("prior service Pod changed")
			}
			if _, err := r.currentGroup(ctx, g); err != nil {
				return err
			}
			uid, rv := pod.UID, pod.ResourceVersion
			if err := r.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *LabGroupReconciler) reconcileGroupLifecycleStart(ctx context.Context, g *lab.LabGroup) (bool, ctrl.Result, error) {
	i := g.Spec.Lifecycle
	adopted := adoptReleasedScopeHistory(g.Status.ServiceRuntime, g.Status.ServiceReports)
	if !reflect.DeepEqual(adopted, g.Status.ServiceRuntime) {
		base := g.DeepCopy()
		g.Status.ServiceRuntime = adopted
		if err := r.Status().Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	if err := r.observeGroupAllocation(ctx, g); err != nil {
		return true, ctrl.Result{}, err
	}
	for _, row := range g.Status.ServiceRuntime {
		if row.OperationID != i.OperationID || row.Revision != i.Revision {
			if !runtimeRowsReleased([]lab.OwnedRuntimeIdentity{row}, g.Status.ServiceReports, row.OwnerUID, row.OperationID, row.Revision) {
				if err := r.drainPriorGroupServices(ctx, g); err != nil {
					return true, ctrl.Result{}, err
				}
				return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, "Starting", "DrainingPriorNativeObligation")
			}
		}
	}
	if err := r.observeGroupAllocation(ctx, g); err != nil {
		return true, ctrl.Result{}, err
	}
	return false, ctrl.Result{}, nil
}
func (r *LabGroupReconciler) scaleStoppedGroupServices(ctx context.Context, g *lab.LabGroup) (bool, ctrl.Result, error) {
	var err error
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
				return true, ctrl.Result{RequeueAfter: 3 * time.Second}, r.groupLifecycleStatus(ctx, g, observationStateUnknown, "WaitingForNativeInventory")
			}
			d.Spec.Replicas = ptrInt32(0)
			if err = r.Update(ctx, &d); err != nil {
				return true, ctrl.Result{}, err
			}
		}
	}
	return false, ctrl.Result{}, nil
}
func priorGroupReleaseRows(live *lab.LabGroup) []lab.OwnedRuntimeIdentity {
	rows := []lab.OwnedRuntimeIdentity{}
	for _, old := range live.Status.ServiceRuntime {
		if old.OperationID == live.Spec.Lifecycle.OperationID && old.Revision == live.Spec.Lifecycle.Revision {
			rows = append(rows, old)
			continue
		}
		released := false
		for _, report := range live.Status.ServiceReports {
			if reflect.DeepEqual(old, report.Identity) && report.RuntimeState == runtimeStateReleased && report.Error == "" && report.ObservedAt != nil && !report.ObservedAt.IsZero() && report.RuntimeAbsentAt != nil && !report.RuntimeAbsentAt.IsZero() && report.CgroupAbsentAt != nil && !report.CgroupAbsentAt.IsZero() && report.AttachmentsAbsentAt != nil && !report.AttachmentsAbsentAt.IsZero() {
				released = true
				break
			}
		}
		if !released {
			rows = append(rows, old)
		}
	}
	return rows
}
func groupReleaseReportMatches(live *lab.LabGroup, report *lab.OwnedRuntimeReport, d *appsv1.Deployment, p *corev1.Pod, node *corev1.Node, component string, ids []string) bool {
	candidate := &report.Identity
	return candidate.OwnerUID == string(live.UID) && candidate.OperationID == live.Spec.Lifecycle.OperationID && candidate.Revision == live.Spec.Lifecycle.Revision && candidate.DeploymentUID == string(d.UID) && candidate.PodUID == string(p.UID) && candidate.NodeName == p.Spec.NodeName && candidate.NodeBootID == node.Status.NodeInfo.BootID && candidate.Component == component && containsStrings(candidate.ContainerIDs, ids) && len(candidate.CgroupPaths) > 0 && (len(candidate.PortKeys) > 0 || candidate.AttachmentsComplete) && report.ObservedAt != nil && report.Error == ""
}
func (r *LabGroupReconciler) inventoryGroupServicePods(ctx context.Context, live *lab.LabGroup, pods corev1.PodList, rows []lab.OwnedRuntimeIdentity) ([]lab.OwnedRuntimeIdentity, bool, error) {
	ns := lab.LabGroupNamespaceOf(live)
	var err error
	for _, p := range pods.Items {
		component := serviceComponent(&p)
		if component == "" {
			continue
		}
		var d appsv1.Deployment
		if err = r.groupReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: component}, &d); err != nil {
			return nil, false, err
		}
		if !r.servicePodOwned(ctx, &p, &d) {
			return nil, false, fmt.Errorf("foreign service Pod %s", p.Name)
		}
		var node corev1.Node
		if p.Spec.NodeName == "" {
			return nil, false, nil
		}
		if err = r.groupReader().Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node); err != nil {
			return nil, false, err
		}
		ids := []string{}
		for _, c := range p.Status.ContainerStatuses {
			if c.ContainerID != "" {
				ids = append(ids, c.ContainerID)
			}
		}
		if len(ids) == 0 || node.Status.NodeInfo.BootID == "" {
			return nil, false, nil
		}
		var identity *lab.OwnedRuntimeIdentity
		for n := range live.Status.ServiceReports {
			report := &live.Status.ServiceReports[n]
			candidate := &report.Identity
			if groupReleaseReportMatches(live, report, &d, &p, &node, component, ids) {
				identity = candidate
				break
			}
		}
		if identity == nil {
			return nil, false, nil
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
	return rows, true, nil
}
func skipPriorGroupServiceDebt(g *lab.LabGroup, row lab.OwnedRuntimeIdentity) bool {
	intent := g.Spec.Lifecycle
	return row.OwnerUID != string(g.UID) || row.OperationID == intent.OperationID && row.Revision == intent.Revision || row.DeploymentUID == "" || row.PodUID == "" || runtimeRowsReleased([]lab.OwnedRuntimeIdentity{row}, g.Status.ServiceReports, row.OwnerUID, row.OperationID, row.Revision)
}

func groupServicePodRecorded(pod *corev1.Pod, rows []lab.OwnedRuntimeIdentity) bool {
	known := false
	for _, row := range rows {
		known = known || string(pod.UID) == row.PodUID
	}
	return known
}
