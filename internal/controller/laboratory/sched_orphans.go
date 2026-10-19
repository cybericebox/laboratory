package laboratory

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// This file is the cleanup of the groups of a Reservation that was deleted (by hand or by the backend) instead of ending: the
// ReservationReconciler never sees its end, so the scheduler takes what still runs under the gone name. A Lab or LabGroup whose
// reservation label names a Reservation that does not exist is never admitted (ReservationMissing); one that has pods out
// already (it ran under the reservation before it was deleted) gets the same notice as an ended reservation and is then deleted.

// passRecord is what one pass of the scheduler saw (when this process first saw each object so, by UID) and when that pass ran.
type passRecord struct {
	at    time.Time
	first map[types.UID]time.Time
}

// recent is the record if its pass ran at most maxAge before now, else nil: a longer gap between two passes breaks the
// observation, so the next sighting starts a new notice.
func (r passRecord) recent(now time.Time, maxAge time.Duration) map[types.UID]time.Time {
	if r.first == nil || now.Sub(r.at) > maxAge {
		return nil
	}
	return r.first
}

// seenMaxAge is the longest gap between two passes after which a sighting record still counts: ten ticks, at least a minute.
func (s *Scheduler) seenMaxAge() time.Duration {
	tick := s.Config.Tick
	if tick <= 0 {
		tick = defaultSchedulerTick
	}
	return max(10*tick, time.Minute)
}

// ranLab says some container device of the lab was dispatched (or has a pod): it ran, it did not only wait.
func ranLab(snap *clusterView, l *laboratoryv1alpha1.Lab) bool {
	for _, t := range l.Spec.Devices {
		if t.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue
		}
		key := devicePodKey(l.Namespace, l.Name, t.Name)
		if len(snap.podsOf[key]) > 0 {
			return true
		}
		if d := snap.devices[key]; d != nil && d.Status.Scheduling != nil && (&schedPod{state: d.Status.Scheduling.State}).dispatched() {
			return true
		}
	}
	return false
}

// ranGroup says a pod of the LabGroup was dispatched.
func ranGroup(g *laboratoryv1alpha1.LabGroup) bool {
	for _, e := range g.Status.Pods {
		if (&schedPod{state: e.State}).dispatched() {
			return true
		}
	}
	return false
}

// orphanTargets are the objects of the view, by objectKey, that ran under a reservation that does not exist now, with that
// reservation's name: Labs with pods out, stopped ones too, and LabGroups with pods out whose namespace holds only labs of
// their own tenant under the same gone reservation (deleting the group drains those; one that holds anything else stays).
// Nothing is a target while the Reservation kind is not served: then the list says nothing about what exists.
func (s *Scheduler) orphanTargets(snap *clusterView) map[string]string {
	out := map[string]string{}
	if s.noReservations {
		return out
	}
	exists := make(map[string]bool, len(snap.reservations))
	for i := range snap.reservations {
		exists[snap.reservations[i].Name] = true
	}
	gone := func(o client.Object) (string, bool) {
		name, _ := names.ReservationOf(o.GetLabels())
		return name, name != "" && !exists[name] && o.GetDeletionTimestamp() == nil
	}
	byNamespace := map[string][]*laboratoryv1alpha1.Lab{}
	for _, l := range snap.labs {
		byNamespace[l.Namespace] = append(byNamespace[l.Namespace], l)
		if name, ok := gone(l); ok && ranLab(snap, l) {
			out[objectKey(l)] = name
		}
	}
	for _, g := range snap.groups {
		name, ok := gone(g)
		if !ok || !ranGroup(g) {
			continue
		}
		if !groupHoldsOtherWork(g, name, byNamespace[laboratoryv1alpha1.LabGroupNamespaceOf(g)]) {
			out[objectKey(g)] = name
		}
	}
	return out
}

// groupHoldsOtherWork says the namespace of the LabGroup holds a lab that is not of the group's tenant under the gone
// reservation name (another tenant's, another reservation's, an unlabelled one or one being deleted): deleting the group would
// drain it.
func groupHoldsOtherWork(g *laboratoryv1alpha1.LabGroup, name string, labs []*laboratoryv1alpha1.Lab) bool {
	t := names.TenantOf(g.Labels)
	for _, l := range labs {
		if r, _ := names.ReservationOf(l.Labels); r != name || names.TenantOf(l.Labels) != t || l.DeletionTimestamp != nil {
			return true
		}
	}
	return false
}

// reservationCRDName is the CustomResourceDefinition of the Reservation kind.
const reservationCRDName = "reservations.laboratory.cybericebox.com"

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=reservations.laboratory.cybericebox.com,verbs=get

// ReservationCRDBy asks the API server, past any cache, whether it serves the Reservation kind now (its discovery) and which
// CustomResourceDefinition serves it (its UID; a reinstalled CRD has a new one): the Scheduler's ReservationCRD. r must read
// past the cache (the manager's API reader).
func ReservationCRDBy(dc discovery.DiscoveryInterface, r client.Reader) func(context.Context) (types.UID, bool, error) {
	return func(ctx context.Context) (types.UID, bool, error) {
		list, err := dc.ServerResourcesForGroupVersion(laboratoryv1alpha1.SchemeGroupVersion.String())
		if errors.IsNotFound(err) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		served := false
		for _, res := range list.APIResources {
			served = served || res.Kind == "Reservation"
		}
		if !served {
			return "", false, nil
		}
		crd := &metav1.PartialObjectMetadata{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		if err := r.Get(ctx, client.ObjectKey{Name: reservationCRDName}, crd); err != nil {
			if errors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, err
		}
		return crd.UID, true, nil
	}
}

// orphanCRDCheck asks ReservationCRD whether the Reservation kind is served by the CRD the sightings started under. A kind not
// served (the CRD deleted: every Reservation went with it, and a Get of the kind is a NotFound too) or served by another CRD
// (deleted and applied again: the reservations did not come back by themselves) is no reservation gone: it clears the
// sightings, so every object starts a new full notice once the kind is back. start: a new chain of sightings begins, under
// the CRD served now. It reports whether the pass may go on.
func (s *Scheduler) orphanCRDCheck(ctx context.Context, start bool) bool {
	if s.ReservationCRD == nil {
		return true
	}
	uid, served, err := s.ReservationCRD(ctx)
	switch {
	case err != nil:
		log.FromContext(ctx).WithName("scheduler").Error(err, "check the Reservation CRD")
		if start {
			s.orphanSeen = passRecord{}
		}
		return false
	case !served:
		s.orphanSeen = passRecord{}
		return false
	case start:
		s.orphanCRD = uid
	case uid != s.orphanCRD:
		log.FromContext(ctx).WithName("scheduler").Info("the Reservation CRD was installed again: the notices of the work of gone reservations start again")
		s.orphanSeen, s.orphanCRD = passRecord{}, uid
		return false
	}
	return true
}

// cleanOrphans gives every orphan target its notice and deletes the targets whose notice ran out. The notice is that of the
// reservations' end, at least a minute (unreservedNotice), counted from when this process first saw the object so; it must
// have been a target on every pass since (seen: the previous pass's record; a gap, a failed pass or a restart starts it
// again). The first sighting is told by a Warning event. Before a deletion the Reservation is read past the cache (it must
// still be gone) and so is the object (the same UID, still labelled with that name); it goes with its UID and resourceVersion
// as precondition. What is not deleted waits evictRetryAfter before the next try.
func (s *Scheduler) cleanOrphans(ctx context.Context, snap *clusterView, seen map[types.UID]time.Time, now time.Time) {
	targets := s.orphanTargets(snap)
	if len(targets) > 0 && len(seen) == 0 && !s.orphanCRDCheck(ctx, true) {
		return
	}
	notice := s.unreservedNotice()
	next := map[types.UID]time.Time{}
	var due []client.Object
	objs := make([]client.Object, 0, len(snap.labs)+len(snap.groups))
	for _, l := range snap.labs {
		objs = append(objs, l)
	}
	for _, g := range snap.groups {
		objs = append(objs, g)
	}
	for _, o := range objs {
		name, ok := targets[objectKey(o)]
		if !ok {
			continue
		}
		uid := o.GetUID()
		first, known := seen[uid]
		if !known {
			first = s.now()
			if s.Recorder != nil {
				s.Recorder.Eventf(o, corev1.EventTypeWarning, "ReservationGone",
					"the reservation %s this object ran under is gone: it is deleted at %s unless the reservation comes back", name, first.Add(notice).UTC().Format(time.RFC3339))
			}
		}
		next[uid] = first
		if !now.Before(first.Add(notice)) && !now.Before(s.orphanRetry[uid]) {
			due = append(due, o)
		}
	}
	s.orphanSeen = passRecord{at: now, first: next}
	for uid := range s.orphanRetry {
		if _, ok := next[uid]; !ok {
			delete(s.orphanRetry, uid)
		}
	}
	if s.orphanRetry == nil {
		s.orphanRetry = map[types.UID]time.Time{}
	}
	if len(due) > 0 && !s.orphanCRDCheck(ctx, false) {
		return
	}
	for _, o := range due {
		s.orphanRetry[o.GetUID()] = now.Add(evictRetryAfter)
		s.deleteOrphan(ctx, o, targets[objectKey(o)])
	}
}

// deleteOrphan deletes a due object of the gone reservation name once the Reservation is confirmed gone and the object (and the
// labs of a LabGroup's namespace) are checked past the cache, with its UID and resourceVersion as precondition.
func (s *Scheduler) deleteOrphan(ctx context.Context, o client.Object, name string) {
	logger := log.FromContext(ctx).WithName("scheduler")
	key := objectKey(o)
	var r laboratoryv1alpha1.Reservation
	if err := s.directReader().Get(ctx, client.ObjectKey{Name: name}, &r); !errors.IsNotFound(err) {
		if err != nil {
			logger.Error(err, "check the reservation of an orphaned object", "object", key, "reservation", name)
		}
		return // it exists (the cache lags) or the check failed
	}
	cur := o.DeepCopyObject().(client.Object)
	if err := s.directReader().Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "check an orphaned object", "object", key)
		}
		return
	}
	if r, _ := names.ReservationOf(cur.GetLabels()); cur.GetUID() != o.GetUID() || r != name || cur.GetDeletionTimestamp() != nil {
		return
	}
	if g, ok := cur.(*laboratoryv1alpha1.LabGroup); ok {
		// the labs of its namespace past the cache too: one the cache does not hold yet would be drained with it
		var labs laboratoryv1alpha1.LabList
		if err := s.directReader().List(ctx, &labs, client.InNamespace(laboratoryv1alpha1.LabGroupNamespaceOf(g))); err != nil {
			logger.Error(err, "check the labs of an orphaned group", "object", key)
			return
		}
		list := make([]*laboratoryv1alpha1.Lab, 0, len(labs.Items))
		for i := range labs.Items {
			list = append(list, &labs.Items[i])
		}
		if groupHoldsOtherWork(g, name, list) {
			return
		}
	}
	uid, rv := cur.GetUID(), cur.GetResourceVersion()
	if err := s.Delete(ctx, cur, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		if !errors.IsNotFound(err) && !errors.IsConflict(err) {
			logger.Error(err, "delete an orphaned object", "object", key)
		}
		return
	}
	logger.Info("deleted work of a reservation that is gone", "object", key, "reservation", name, "tenant", names.TenantOf(cur.GetLabels()))
}
