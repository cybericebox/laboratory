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
	// grpc holds the Services of the gRPC access of the lab's devices by name; they are not the web Service of a device.
	grpc map[string]*corev1.Service
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

// grpcServices returns the gRPC access Services of the lab, from the same single list as find.
func (s *labServiceSnapshot) grpcServices(ctx context.Context) ([]*corev1.Service, error) {
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	out := make([]*corev1.Service, 0, len(s.grpc))
	for _, svc := range s.grpc {
		out = append(out, svc.DeepCopy())
	}
	return out, nil
}

func (s *labServiceSnapshot) load(ctx context.Context) error {
	if !s.loaded {
		var list corev1.ServiceList
		if err := s.reader.List(ctx, &list, client.InNamespace(s.lab.Namespace), client.MatchingLabels{names.LabelLab: s.lab.Name}); err != nil {
			return err
		}
		s.byDevice = map[string]*corev1.Service{}
		for i := range list.Items {
			s.remember(&list.Items[i])
		}
		s.loaded = true
	}
	return nil
}

func (s *labServiceSnapshot) find(ctx context.Context, device string) (*corev1.Service, error) {
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	if svc := s.byDevice[device]; svc != nil {
		return svc.DeepCopy(), nil
	}
	return nil, nil
}

func (s *labServiceSnapshot) remember(svc *corev1.Service) {
	// The Service of another access kind (the gRPC one) is not the web Service of the device and has its own host.
	if svc.Namespace != s.lab.Namespace || svc.Labels[names.LabelLab] != s.lab.Name || !ownedByLab(svc, s.lab) {
		return
	}
	if svc.Labels[names.LabelAccessKind] == names.AccessKindGRPC {
		if s.grpc == nil {
			s.grpc = map[string]*corev1.Service{}
		}
		s.grpc[svc.Name] = svc.DeepCopy()
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
