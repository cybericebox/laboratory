package l7

import (
	"context"
	"fmt"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type AccessGroup struct{ Namespace, Tenant string }
type AccessRoute struct{ URL, Lab string }

// AccessReader uses the proxy's compact informer cache, without a permission
// TTL. A request reads its group once and its route Service once.
type AccessReader struct{ Reader client.Reader }

func (a *AccessReader) Group(ctx context.Context, id string) (AccessGroup, error) {
	var g lab.LabGroup
	if err := a.Reader.Get(ctx, types.NamespacedName{Name: names.EncodeName(id)}, &g); err != nil {
		return AccessGroup{}, err
	}
	if !g.DeletionTimestamp.IsZero() {
		return AccessGroup{}, fmt.Errorf("group is deleting")
	}
	return AccessGroup{lab.LabGroupNamespaceOf(&g), names.TenantOf(g.Labels)}, nil
}
func (a *AccessReader) Route(ctx context.Context, task, namespace string) (AccessRoute, error) {
	var svc corev1.Service
	if err := a.Reader.Get(ctx, types.NamespacedName{Name: task, Namespace: namespace}, &svc); err != nil {
		return AccessRoute{}, err
	}
	if !svc.DeletionTimestamp.IsZero() || svc.Labels[names.LabelLab] == "" || svc.Labels[names.LabelDevice] == "" || len(svc.Spec.Ports) == 0 {
		return AccessRoute{}, fmt.Errorf("service is not an exposed lab device")
	}
	protocol := svc.Spec.Ports[0].Name
	if protocol == "" {
		protocol = "http"
	}
	port := 80
	if protocol == "https" {
		port = 443
	}
	return AccessRoute{fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", protocol, task, namespace, port), svc.Labels[names.LabelLab]}, nil
}
func (a *AccessReader) Allowed(ctx context.Context, namespace, member, labName string) bool {
	var target lab.Lab
	if err := a.Reader.Get(ctx, types.NamespacedName{Name: labName, Namespace: namespace}, &target); err != nil || !target.DeletionTimestamp.IsZero() || target.Spec.Lifecycle.IsStopped() {
		return false
	}
	var c lab.LabGroupClient
	if err := a.Reader.Get(ctx, types.NamespacedName{Name: member, Namespace: namespace}, &c); err != nil || !c.DeletionTimestamp.IsZero() {
		return false
	}
	var policy lab.LabGroupAccessPolicy
	if err := a.Reader.Get(ctx, types.NamespacedName{Name: names.LabGroupAccessPolicyName, Namespace: namespace}, &policy); err != nil || !policy.DeletionTimestamp.IsZero() {
		return false
	}
	return PolicyAllows(policy.Spec.Rules, member, labName)
}
func (a *AccessReader) Tenant(id string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g, err := a.Group(ctx, id)
	return g.Tenant, err == nil
}
func (h *Handler) WithAccessReader(a *AccessReader) *Handler {
	h.access = a
	h.groupTenant = a.Tenant
	return h
}
func (h *Handler) WithReportPreparation(prepare func(context.Context, string) error) *Handler {
	h.prepare = prepare
	return h
}

// CompactCacheObject preserves only what proxy authorization/routing uses.
// It owns its copy; objects supplied by informer transforms remain untouched.
func CompactCacheObject(in any) (any, error) {
	var out client.Object
	switch x := in.(type) {
	case *lab.LabGroup:
		g := x.DeepCopy()
		namespace := g.Status.Namespace
		g.Spec = lab.LabGroupSpec{}
		g.Status = lab.LabGroupStatus{Namespace: namespace}
		out = g
	case *lab.Lab:
		l := x.DeepCopy()
		l.Spec = lab.LabSpec{Lifecycle: l.Spec.Lifecycle}
		l.Status = lab.LabStatus{}
		out = l
	case *lab.LabGroupClient:
		c := x.DeepCopy()
		c.Status = lab.LabGroupClientStatus{}
		c.Spec = lab.LabGroupClientSpec{}
		out = c
	case *lab.LabGroupAccessPolicy:
		out = x.DeepCopy()
	case *corev1.Service:
		s := x.DeepCopy()
		ports := s.Spec.Ports
		s.Spec = corev1.ServiceSpec{Ports: ports}
		s.Status = corev1.ServiceStatus{}
		out = s
	case *corev1.Secret:
		out = x.DeepCopy()
	default:
		return in, nil
	}
	out.SetManagedFields(nil)
	return out, nil
}
