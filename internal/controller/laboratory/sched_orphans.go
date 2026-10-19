package laboratory

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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
		t, kept := names.TenantOf(g.Labels), false
		for _, l := range byNamespace[laboratoryv1alpha1.LabGroupNamespaceOf(g)] {
			if r, _ := names.ReservationOf(l.Labels); r != name || names.TenantOf(l.Labels) != t || l.DeletionTimestamp != nil {
				kept = true
			}
		}
		if !kept {
			out[objectKey(g)] = name
		}
	}
	return out
}

// cleanOrphans gives every orphan target its notice and deletes the targets whose notice ran out. The notice is that of the
// reservations' end, at least a minute (unreservedNotice), counted from when this process first saw the object so; it must
// have been a target on every pass since (seen: the previous pass's record; a gap, a failed pass or a restart starts it
// again). The first sighting is told by a Warning event. Before a deletion the Reservation is read past the cache (it must
// still be gone) and so is the object (the same UID, still labelled with that name); it goes with its UID and resourceVersion
// as precondition. What is not deleted waits evictRetryAfter before the next try.
func (s *Scheduler) cleanOrphans(ctx context.Context, snap *clusterView, seen map[types.UID]time.Time, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	targets := s.orphanTargets(snap)
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
	for _, o := range due {
		key, name := objectKey(o), targets[objectKey(o)]
		s.orphanRetry[o.GetUID()] = now.Add(evictRetryAfter)
		var r laboratoryv1alpha1.Reservation
		if err := s.directReader().Get(ctx, client.ObjectKey{Name: name}, &r); !errors.IsNotFound(err) {
			if err != nil {
				logger.Error(err, "check the reservation of an orphaned object", "object", key, "reservation", name)
			}
			continue // it exists (the cache lags) or the check failed
		}
		cur := o.DeepCopyObject().(client.Object)
		if err := s.directReader().Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "check an orphaned object", "object", key)
			}
			continue
		}
		if r, _ := names.ReservationOf(cur.GetLabels()); cur.GetUID() != o.GetUID() || r != name || cur.GetDeletionTimestamp() != nil {
			continue
		}
		uid, rv := cur.GetUID(), cur.GetResourceVersion()
		if err := s.Delete(ctx, cur, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
			if !errors.IsNotFound(err) && !errors.IsConflict(err) {
				logger.Error(err, "delete an orphaned object", "object", key)
			}
			continue
		}
		logger.Info("deleted work of a reservation that is gone", "object", key, "reservation", name, "tenant", names.TenantOf(cur.GetLabels()))
	}
}
