package laboratory

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// The scheduler decides when a group's VPN and gateway pods start. Status.Pods
// holds one record per pod: the reconciler adds it (Queued when the Deployment
// does not exist yet, Started when it does, so a group that predates the
// scheduler is left alone) and the scheduler moves it on.

// groupPodQueued reports whether the scheduler still holds the named pod back.
func (r *LabGroupReconciler) groupPodQueued(lg *laboratoryv1alpha1.LabGroup, name string) bool {
	if !r.Scheduled {
		return false
	}
	for _, p := range lg.Status.Pods {
		if p.Name == name {
			return p.State == laboratoryv1alpha1.PodQueued
		}
	}
	return false
}

// ensureGroupScheduling adds the record of every pod the group runs that has none.
func (r *LabGroupReconciler) ensureGroupScheduling(ctx context.Context, lg *laboratoryv1alpha1.LabGroup) error {
	if !r.Scheduled {
		return nil
	}
	ns := laboratoryv1alpha1.LabGroupNamespaceOf(lg)
	now := metav1.Now()
	added := false
	for _, name := range groupPodNames(lg) {
		have := false
		for _, p := range lg.Status.Pods {
			have = have || p.Name == name
		}
		if have {
			continue
		}
		entry := laboratoryv1alpha1.NamedPodSchedule{Name: name, PodSchedule: laboratoryv1alpha1.PodSchedule{
			State: laboratoryv1alpha1.PodQueued, QueuedAt: &now,
		}}
		var dep appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &dep)
		switch {
		case err == nil:
			entry.PodSchedule = laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodStarted, StartedAt: &now}
		case !errors.IsNotFound(err):
			return err
		}
		lg.Status.Pods = append(lg.Status.Pods, entry)
		added = true
	}
	if !added {
		return nil
	}
	return r.Status().Update(ctx, lg)
}

// syncPodLabels keeps the user labels of the group's VPN and gateway pods equal
// to those of the LabGroup. Only pod metadata changes, never the template, so a
// label change restarts nothing; a pod created later is brought up to date on the
// next reconcile.
func (r *LabGroupReconciler) syncPodLabels(ctx context.Context, lg *laboratoryv1alpha1.LabGroup) error {
	desired := userLabels(lg.Labels)
	for _, component := range []string{names.ComponentVPN, names.ComponentGateway} {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(laboratoryv1alpha1.LabGroupNamespaceOf(lg)), client.MatchingLabels{names.LabelComponent: component}); err != nil {
			return err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.DeletionTimestamp != nil {
				continue
			}
			orig := p.DeepCopy()
			if applyUserLabels(p, desired) {
				if err := r.Patch(ctx, p, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		}
	}
	return nil
}
