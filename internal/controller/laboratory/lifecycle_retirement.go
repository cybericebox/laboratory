package laboratory

import (
	"context"
	"errors"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/snapshot"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type retirementCatalog interface {
	SnapshotCatalog
	DeleteSnapshot(context.Context, string, string) error
	RepositoryEmpty(context.Context, string) (bool, error)
}

func GroupRetirementReady(g *lab.LabGroup) bool {
	i, o, a := g.Spec.Lifecycle, g.Status.Lifecycle, g.Status.Resources
	return i != nil && i.IsStopped() && g.Spec.Admission == nil && o != nil && a != nil && o.LabUID == string(g.UID) && o.OperationID == i.OperationID && o.Revision == i.Revision && o.ObservedGeneration == g.Generation && o.ObservedState == "Stopped" && o.Error == "" && nonzeroTime(o.StoppedAt) && a.OperationID == i.OperationID && a.Revision == i.Revision && a.RuntimeState == "Released" && nonzeroTime(a.ObservedAt) && nonzeroTime(a.ReleasedAt) && a.AllocatedRequests == (lab.ResourceAmounts{})
}
func retirementMatches(in lab.LifecycleRetirementIntent, status *lab.LifecycleRetirementStatus, generation int64) bool {
	return status != nil && status.ExpectedUID == in.ExpectedUID && status.StopOperationID == in.StopOperationID && status.StopRevision == in.StopRevision && status.OperationID == in.OperationID && status.Revision == in.Revision && status.ObservedGeneration == generation && nonzeroTime(status.RequestedAt)
}
func LifecycleRetired(l *lab.Lab) bool {
	in, valid := lab.ParseLifecycleRetirement(l.Annotations[names.AnnotationLifecycleRetirement])
	o := l.Status.Retirement
	return valid && in.ExpectedUID == string(l.UID) && retirementMatches(in, o, l.Generation) && o.State == "Deleted" && o.RuntimeAbsent && o.CleanupComplete && o.StorageState == "Deleted" && o.Error == "" && nonzeroTime(o.ObservedAt) && o.ObservedAt.After(o.RequestedAt.Time)
}
func freshRetirementRows(rows []lab.OwnedRuntimeIdentity, reports []lab.OwnedRuntimeReport, in lab.LifecycleRetirementIntent) bool {
	if len(rows) == 0 {
		return false
	}
	for _, id := range rows {
		// Retirement challenges cover retained original obligations as well as
		// the current stop. Their immutable tuples must never be rebound.
		scope := id.ScopeKind == "LabFabric" || id.ScopeKind == "GroupScope" || id.ScopeKind == "NeverMaterialized"
		if id.OwnerUID != in.ExpectedUID || id.OperationID == "" || id.Revision < 1 || id.Revision > in.StopRevision || id.Revision == in.StopRevision && id.OperationID != in.StopOperationID || in.Generation > 0 && (id.Generation < 1 || id.Generation > in.Generation) || id.NodeName == "" || id.NodeBootID == "" || scope && (id.ScopeUID == "" || id.ScopeKind != "NeverMaterialized" && id.ScopeUID != in.ExpectedUID || !id.AttachmentsComplete) || !scope && (id.PodUID == "" || len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 || len(id.PortKeys) == 0 && !id.AttachmentsComplete) {
			return false
		}
		found := false
		for _, report := range reports {
			if reflect.DeepEqual(id, report.Identity) && report.RetirementOperationID == in.OperationID && report.RetirementRevision == in.Revision && report.RuntimeState == "Released" && report.Error == "" && nonzeroTime(report.ObservedAt) && nonzeroTime(report.RuntimeAbsentAt) && nonzeroTime(report.CgroupAbsentAt) && nonzeroTime(report.AttachmentsAbsentAt) {
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
func retirementObservation(in lab.LifecycleRetirementIntent, generation int64, previous ...*lab.LifecycleRetirementStatus) *lab.LifecycleRetirementStatus {
	now := metav1.Now()
	// The agent acceptance timestamp is never ordered against the independent
	// operator/node clocks. Persist the operator's own first receipt time.
	requested := now.DeepCopy()
	if len(previous) > 0 && previous[0] != nil && retirementMatches(in, previous[0], previous[0].ObservedGeneration) {
		requested = previous[0].RequestedAt.DeepCopy()
	}
	return &lab.LifecycleRetirementStatus{ExpectedUID: in.ExpectedUID, StopOperationID: in.StopOperationID, StopRevision: in.StopRevision, OperationID: in.OperationID, Revision: in.Revision, ObservedGeneration: generation, State: "CleanupPending", StorageState: "CleanupPending", RequestedAt: requested, ObservedAt: &now}
}
func (s *RetentionSweeper) SweepLifecycleRetirements(ctx context.Context) error {
	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	var labs lab.LabList
	if err := reader.List(ctx, &labs); err != nil {
		return err
	}
	var errs []error
	for i := range labs.Items {
		l := &labs.Items[i]
		if l.Annotations[names.AnnotationLifecycleRetirement] != "" {
			if err := s.retireLifecycleLab(ctx, reader, l); err != nil {
				errs = append(errs, fmt.Errorf("Lab %s/%s retirement: %w", l.Namespace, l.Name, err))
			}
		}
	}
	var groups lab.LabGroupList
	if err := reader.List(ctx, &groups); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for i := range groups.Items {
		g := &groups.Items[i]
		if g.Annotations[names.AnnotationLifecycleRetirement] != "" {
			if err := s.retireLifecycleGroup(ctx, reader, g); err != nil {
				errs = append(errs, fmt.Errorf("Group %s retirement: %w", g.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}
func (s *RetentionSweeper) retireLifecycleLab(ctx context.Context, reader client.Reader, l *lab.Lab) error {
	in, valid := lab.ParseLifecycleRetirement(l.Annotations[names.AnnotationLifecycleRetirement])
	if l.Annotations[names.AnnotationLabVariableAdmission] != "" {
		return fmt.Errorf("Lab variable write is pending")
	}
	if !valid || in.ExpectedUID != string(l.UID) || !l.DeletionTimestamp.IsZero() || l.Spec.Lifecycle == nil || in.StopOperationID != l.Spec.Lifecycle.OperationID || in.StopRevision != l.Spec.Lifecycle.Revision {
		return fmt.Errorf("retirement identity changed")
	}
	if LifecycleRetired(l) {
		return nil
	}
	next := retirementObservation(in, l.Generation, l.Status.Retirement)
	pendingCertified := l.Status.Retirement != nil && retirementMatches(in, l.Status.Retirement, l.Status.Retirement.ObservedGeneration) && l.Status.Retirement.ObservedGeneration >= in.Generation && l.Status.Retirement.ObservedGeneration <= l.Generation && l.Status.Retirement.RuntimeAbsent && l.Status.Retirement.StorageState == "Deleted" && l.Status.Retirement.Error == ""
	var devices lab.DeviceList
	if err := reader.List(ctx, &devices, client.InNamespace(l.Namespace)); err != nil {
		return err
	}
	if !pendingCertified {
		if l.Generation != in.Generation || !RetirementReady(l) {
			return s.patchLabRetirement(ctx, l, next, fmt.Errorf("exact original stop certificate unavailable"))
		}
		rows := append([]lab.OwnedRuntimeIdentity(nil), l.Status.ScopeInventory...)
		reports := append([]lab.OwnedRuntimeReport(nil), l.Status.ScopeReports...)
		for i := range devices.Items {
			if ownedLabDevice(l, &devices.Items[i]) {
				rows = append(rows, devices.Items[i].Status.RuntimeInventory...)
				reports = append(reports, devices.Items[i].Status.RuntimeReports...)
			}
		}
		if !freshRetirementRows(rows, reports, in) {
			return s.patchLabRetirement(ctx, l, next, fmt.Errorf("waiting for native retirement challenge acknowledgement"))
		}
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(l.Namespace), client.MatchingLabels{names.LabelLab: l.Name}); err != nil {
			return err
		}
		if len(pods.Items) > 0 {
			return s.patchLabRetirement(ctx, l, next, fmt.Errorf("child Pod remains"))
		}
		catalog, ok := s.Registry.(retirementCatalog)
		if !ok {
			return s.patchLabRetirement(ctx, l, next, fmt.Errorf("snapshot absence observer unavailable"))
		}
		repos, err := catalog.Repos(ctx)
		if err != nil {
			return s.patchLabRetirement(ctx, l, next, err)
		}
		owned := map[string]bool{}
		for _, repo := range repos {
			if key, ok := labKeyOf(repo); ok && key == l.Namespace+"_"+l.Name {
				owned[repo] = true
			}
		}
		for i := range devices.Items {
			d := &devices.Items[i]
			if !ownedLabDevice(l, d) {
				continue
			}
			repo := snapshot.Repo(l.Namespace, l.Name, d.Spec.Name)
			owned[repo] = true
			if d.Status.State != nil {
				if err := catalog.DeleteSnapshot(ctx, repo, d.Status.State.Image); err != nil {
					return s.patchLabRetirement(ctx, l, next, err)
				}
			}
		}
		for repo := range owned {
			if err := s.checkLabRetirement(ctx, reader, l, in); err != nil {
				return err
			}
			if err := catalog.DeleteRepo(ctx, repo); err != nil {
				return s.patchLabRetirement(ctx, l, next, err)
			}
			empty, err := catalog.RepositoryEmpty(ctx, repo)
			if err != nil {
				return s.patchLabRetirement(ctx, l, next, err)
			}
			if !empty {
				return s.patchLabRetirement(ctx, l, next, fmt.Errorf("snapshot manifests remain"))
			}
		}
		objects, err := s.captureRetirementObjects(ctx, reader, l.Namespace, string(l.UID), false)
		if err != nil {
			return err
		}
		next.Objects = objects
		next.RuntimeAbsent = true
		next.StorageState = "Deleted"
		// Durable intermediate receipt preserves native+manifest proof across a
		// crash while the now-unneeded owned configuration objects are removed.
		if err := s.patchLabRetirement(ctx, l, next, nil); err != nil {
			return err
		}
	}
	if err := s.checkLabRetirement(ctx, reader, l, in); err != nil {
		return err
	}
	if err := s.cleanupRetiredLab(ctx, reader, l, &devices); err != nil {
		return err
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		return err
	}
	// Scrubbing a spec changes generation but cannot start runtime: stopped intent
	// and permanent admission fence remain. Reissue stop at the scrubbed generation
	// from the durable NEW native challenge receipt, preserving original op/rev.
	next = retirementObservation(in, l.Generation, l.Status.Retirement)
	next.RuntimeAbsent = true
	next.StorageState = "Deleted"
	next.State = "Deleted"
	next.CleanupComplete = true
	if !next.ObservedAt.After(next.RequestedAt.Time) {
		return nil
	}
	base := l.DeepCopy()
	l.Status.Retirement = next
	if l.Status.Lifecycle != nil {
		l.Status.Lifecycle.ObservedGeneration = l.Generation
	}
	if l.Status.Resources != nil {
		l.Status.Resources.StorageState = "Deleted"
		l.Status.Resources.SnapshotQuotaBytes = 0
		l.Status.Resources.PhysicalStorageBytesAvailable = false
		l.Status.Resources.PhysicalStorageBytes = 0
	}
	l.Status.Devices = nil
	l.Status.Connections = nil
	l.Status.Access = nil
	l.Status.ImageDigests = nil
	l.Status.ImageCache = nil
	l.Status.ImageWarning = ""
	l.Status.VPN = lab.LabNetworkStatus{}
	l.Status.Internet = lab.LabNetworkStatus{}
	l.Status.Conditions = nil
	return s.Client.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
func (s *RetentionSweeper) checkLabRetirement(ctx context.Context, reader client.Reader, l *lab.Lab, in lab.LifecycleRetirementIntent) error {
	var live lab.Lab
	if err := reader.Get(ctx, client.ObjectKeyFromObject(l), &live); err != nil {
		return err
	}
	current, valid := lab.ParseLifecycleRetirement(live.Annotations[names.AnnotationLifecycleRetirement])
	if !valid || !reflect.DeepEqual(current, in) || live.UID != l.UID || !live.DeletionTimestamp.IsZero() || live.Spec.Lifecycle == nil || live.Spec.Lifecycle.OperationID != in.StopOperationID || live.Spec.Lifecycle.Revision != in.StopRevision {
		return fmt.Errorf("retirement fence changed")
	}
	return nil
}
func (s *RetentionSweeper) patchLabRetirement(ctx context.Context, l *lab.Lab, next *lab.LifecycleRetirementStatus, cause error) error {
	if cause != nil {
		next.Error = cause.Error()
	}
	base := l.DeepCopy()
	l.Status.Retirement = next
	if err := s.Client.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return cause
}
func (s *RetentionSweeper) cleanupRetiredLab(ctx context.Context, reader client.Reader, l *lab.Lab, _ *lab.DeviceList) error {
	in, _ := lab.ParseLifecycleRetirement(l.Annotations[names.AnnotationLifecycleRetirement])
	if err := s.cleanupRetirementObjects(ctx, reader, l.Namespace, l.Status.Retirement.Objects, func() error { return s.checkLabRetirement(ctx, reader, l, in) }); err != nil {
		return err
	}
	remaining, err := s.captureRetirementObjects(ctx, reader, l.Namespace, string(l.UID), false)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("unrecorded or pending Lab configuration remains")
	}
	if len(l.Spec.Devices) > 0 || len(l.Spec.Connections) > 0 || l.Spec.VPN.Enabled || l.Spec.Internet.Enabled {
		base := l.DeepCopy()
		l.Spec.Devices = nil
		l.Spec.Connections = nil
		l.Spec.VPN = lab.LabNetworkSpec{}
		l.Spec.Internet = lab.LabNetworkSpec{}
		return s.Client.Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}

func objectOwnedBy(o client.Object, uid string) bool {
	for _, owner := range o.GetOwnerReferences() {
		if string(owner.UID) == uid {
			return true
		}
	}
	return false
}
func (s *RetentionSweeper) retireLifecycleGroup(ctx context.Context, reader client.Reader, g *lab.LabGroup) error {
	in, valid := lab.ParseLifecycleRetirement(g.Annotations[names.AnnotationLifecycleRetirement])
	if !valid || in.ExpectedUID != string(g.UID) || !g.DeletionTimestamp.IsZero() || g.Spec.Lifecycle == nil || g.Spec.Lifecycle.OperationID != in.StopOperationID || g.Spec.Lifecycle.Revision != in.StopRevision {
		return fmt.Errorf("group retirement identity changed")
	}
	if retirementMatches(in, g.Status.Retirement, g.Generation) && g.Status.Retirement.State == "Deleted" && g.Status.Retirement.CleanupComplete {
		return nil
	}
	next := retirementObservation(in, g.Generation, g.Status.Retirement)
	pendingCertified := g.Status.Retirement != nil && retirementMatches(in, g.Status.Retirement, g.Status.Retirement.ObservedGeneration) && g.Status.Retirement.ObservedGeneration >= in.Generation && g.Status.Retirement.ObservedGeneration <= g.Generation && g.Status.Retirement.RuntimeAbsent && g.Status.Retirement.StorageState == "Deleted" && g.Status.Retirement.Error == ""
	var children lab.LabList
	if err := reader.List(ctx, &children, client.InNamespace(lab.LabGroupNamespaceOf(g))); err != nil {
		return err
	}
	for i := range children.Items {
		if !LifecycleRetired(&children.Items[i]) {
			return s.patchGroupRetirement(ctx, g, next, fmt.Errorf("child retirement remains incomplete"))
		}
	}
	if !pendingCertified {
		if g.Generation != in.Generation || !GroupRetirementReady(g) || !freshRetirementRows(g.Status.ServiceRuntime, g.Status.ServiceReports, in) {
			return s.patchGroupRetirement(ctx, g, next, fmt.Errorf("waiting for exact native group retirement challenge"))
		}
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(lab.LabGroupNamespaceOf(g))); err != nil {
			return err
		}
		if len(pods.Items) > 0 {
			return s.patchGroupRetirement(ctx, g, next, fmt.Errorf("group Pod remains"))
		}
		if err := s.checkGroupRetirement(ctx, reader, g, in); err != nil {
			return err
		}
		objects, err := s.captureRetirementObjects(ctx, reader, lab.LabGroupNamespaceOf(g), string(g.UID), true)
		if err != nil {
			return err
		}
		next.Objects = objects
		next.RuntimeAbsent = true
		next.StorageState = "Deleted"
		if err := s.patchGroupRetirement(ctx, g, next, nil); err != nil {
			return err
		}
	}
	if err := s.checkGroupRetirement(ctx, reader, g, in); err != nil {
		return err
	}
	if err := s.cleanupRetirementObjects(ctx, reader, lab.LabGroupNamespaceOf(g), g.Status.Retirement.Objects, func() error { return s.checkGroupRetirement(ctx, reader, g, in) }); err != nil {
		return err
	}
	remaining, err := s.captureRetirementObjects(ctx, reader, lab.LabGroupNamespaceOf(g), string(g.UID), true)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("unrecorded or pending group configuration remains")
	}
	if g.Spec.VPN.KeypairSecretRef != nil {
		base := g.DeepCopy()
		g.Spec.VPN.KeypairSecretRef = nil
		if err := s.Client.Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		return err
	}
	next = retirementObservation(in, g.Generation, g.Status.Retirement)
	next.RuntimeAbsent = true
	next.StorageState = "Deleted"
	next.State = "Deleted"
	next.CleanupComplete = true
	if !next.ObservedAt.After(next.RequestedAt.Time) {
		return nil
	}
	base := g.DeepCopy()
	g.Status.Retirement = next
	g.Status.ServiceRuntime = nil
	g.Status.ServiceReports = nil
	g.Status.VPN = lab.LabGroupVPNStatus{}
	if g.Status.Lifecycle != nil {
		g.Status.Lifecycle.ObservedGeneration = g.Generation
	}
	if g.Status.Resources != nil {
		g.Status.Resources.StorageState = "Deleted"
		g.Status.Resources.SnapshotQuotaBytes = 0
		g.Status.Resources.PhysicalStorageBytesAvailable = false
		g.Status.Resources.PhysicalStorageBytes = 0
	}
	return s.Client.Status().Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
func (s *RetentionSweeper) patchGroupRetirement(ctx context.Context, g *lab.LabGroup, next *lab.LifecycleRetirementStatus, cause error) error {
	if cause != nil {
		next.Error = cause.Error()
	}
	base := g.DeepCopy()
	g.Status.Retirement = next
	if err := s.Client.Status().Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return cause
}
func (s *RetentionSweeper) checkGroupRetirement(ctx context.Context, reader client.Reader, g *lab.LabGroup, in lab.LifecycleRetirementIntent) error {
	var live lab.LabGroup
	if err := reader.Get(ctx, client.ObjectKeyFromObject(g), &live); err != nil {
		return err
	}
	current, valid := lab.ParseLifecycleRetirement(live.Annotations[names.AnnotationLifecycleRetirement])
	if !valid || !reflect.DeepEqual(current, in) || live.UID != g.UID || !live.DeletionTimestamp.IsZero() || live.Spec.Lifecycle == nil || live.Spec.Lifecycle.OperationID != in.StopOperationID || live.Spec.Lifecycle.Revision != in.StopRevision {
		return fmt.Errorf("group retirement fence changed")
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: lab.LabGroupNamespaceOf(&live)}, &ns); err != nil {
		return err
	}
	if string(ns.UID) != in.NamespaceUID || ns.Labels[names.LabelGroup] != live.Name {
		return fmt.Errorf("group namespace UID changed")
	}
	return nil
}

// Capture only object identities after native+manifest proof. The write-only
// agent is never granted Secret read access; the operator owns this inventory.
func (s *RetentionSweeper) captureRetirementObjects(ctx context.Context, reader client.Reader, namespace, uid string, group bool) ([]lab.RetirementObjectIdentity, error) {
	var out []lab.RetirementObjectIdentity
	add := func(kind string, o client.Object) {
		out = append(out, lab.RetirementObjectIdentity{Kind: kind, Name: o.GetName(), UID: string(o.GetUID()), OwnerUID: uid, GroupNamespaceOwned: group})
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range secrets.Items {
		if group || objectOwnedBy(&secrets.Items[i], uid) {
			add("Secret", &secrets.Items[i])
		}
	}
	if group {
		var clients lab.LabGroupClientList
		if err := reader.List(ctx, &clients, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range clients.Items {
			add("LabGroupClient", &clients.Items[i])
		}
		var policies lab.LabGroupAccessPolicyList
		if err := reader.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range policies.Items {
			add("LabGroupAccessPolicy", &policies.Items[i])
		}
		var deployments appsv1.DeploymentList
		if err := reader.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range deployments.Items {
			d := &deployments.Items[i]
			if serviceComponent(&corev1.Pod{ObjectMeta: d.Spec.Template.ObjectMeta}) != "" {
				if err := checkServiceGroupUID(d, uid); err != nil {
					return nil, err
				}
				add("Deployment", d)
			}
		}
	} else {
		var devices lab.DeviceList
		if err := reader.List(ctx, &devices, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range devices.Items {
			if objectOwnedBy(&devices.Items[i], uid) {
				add("Device", &devices.Items[i])
			}
		}
		var connections lab.ConnectionList
		if err := reader.List(ctx, &connections, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		for i := range connections.Items {
			if objectOwnedBy(&connections.Items[i], uid) {
				add("Connection", &connections.Items[i])
			}
		}
	}
	return out, nil
}
func (s *RetentionSweeper) cleanupRetirementObjects(ctx context.Context, reader client.Reader, namespace string, objects []lab.RetirementObjectIdentity, guard func() error) error {
	for _, id := range objects {
		if id.UID == "" || id.OwnerUID == "" {
			return fmt.Errorf("incomplete cleanup ownership receipt")
		}
		var object client.Object
		switch id.Kind {
		case "Secret":
			object = &corev1.Secret{}
		case "Device":
			object = &lab.Device{}
		case "Connection":
			object = &lab.Connection{}
		case "LabGroupClient":
			object = &lab.LabGroupClient{}
		case "LabGroupAccessPolicy":
			object = &lab.LabGroupAccessPolicy{}
		case "Deployment":
			object = &appsv1.Deployment{}
		default:
			return fmt.Errorf("unknown cleanup object kind")
		}
		if err := guard(); err != nil {
			return err
		}
		key := client.ObjectKey{Namespace: namespace, Name: id.Name}
		if err := reader.Get(ctx, key, object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if string(object.GetUID()) != id.UID {
			return fmt.Errorf("replacement cleanup object remains untouched")
		}
		if !id.GroupNamespaceOwned {
			if !objectOwnedBy(object, id.OwnerUID) {
				return fmt.Errorf("cleanup object owner changed")
			}
		}
		uid, rv := object.GetUID(), object.GetResourceVersion()
		if err := s.Client.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := reader.Get(ctx, key, object); err == nil {
			return fmt.Errorf("waiting for exact owned cleanup object deletion")
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
