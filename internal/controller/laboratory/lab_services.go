package laboratory

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// labServiceSnapshot belongs to one reconcile. It keeps successful server
// answers available before the informer catches up, without caching a host
// across reconciles or replacing the namespace-wide code uniqueness reads.
type labServiceSnapshot struct {
	reader   client.Reader
	lab      *api.Lab
	loaded   bool
	byDevice map[string]*corev1.Service
}

func (r *LabReconciler) serviceSnapshot(lab *api.Lab, snapshots ...*labServiceSnapshot) *labServiceSnapshot {
	if len(snapshots) > 0 && snapshots[0] != nil {
		s := snapshots[0]
		if s.lab.Namespace == lab.Namespace && s.lab.Name == lab.Name && s.lab.UID == lab.UID {
			return s
		}
	}
	return &labServiceSnapshot{reader: r.reader(), lab: lab}
}

func (s *labServiceSnapshot) find(ctx context.Context, device string) (*corev1.Service, error) {
	if !s.loaded {
		var list corev1.ServiceList
		if err := s.reader.List(ctx, &list, client.InNamespace(s.lab.Namespace), client.MatchingLabels{names.LabelLab: s.lab.Name}); err != nil {
			return nil, err
		}
		s.byDevice = map[string]*corev1.Service{}
		for i := range list.Items {
			s.remember(&list.Items[i])
		}
		s.loaded = true
	}
	if svc := s.byDevice[device]; svc != nil {
		return svc.DeepCopy(), nil
	}
	return nil, nil
}

func (s *labServiceSnapshot) remember(svc *corev1.Service) {
	if svc.Namespace != s.lab.Namespace || svc.Labels[names.LabelLab] != s.lab.Name || !ownedByLab(svc, s.lab) {
		return
	}
	if s.byDevice == nil {
		s.byDevice = map[string]*corev1.Service{}
	}
	device := svc.Labels[names.LabelDevice]
	old := s.byDevice[device]
	if old == nil || old.Name == svc.Name || svc.CreationTimestamp.Before(&old.CreationTimestamp) || (svc.CreationTimestamp.Equal(&old.CreationTimestamp) && svc.Name < old.Name) {
		s.byDevice[device] = svc.DeepCopy()
	}
}
