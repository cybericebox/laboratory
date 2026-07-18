package grpc

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// CreateLab creates a new Lab custom resource in the given namespace.
func (h *Handler) CreateLab(ctx context.Context, in *protobuf.Lab) (*protobuf.Lab, error) {
	lab, err := protoToLab(in)
	if err != nil {
		return nil, err
	}
	out, err := h.cs.LaboratoryV1alpha1().Labs(in.Namespace).Create(ctx, lab, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if err := h.reconcileEnvSecrets(ctx, out, in.Env); err != nil {
		return nil, err
	}
	return labToProto(out), nil
}

// GetLab fetches a Lab custom resource by namespace and name.
func (h *Handler) GetLab(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.Lab, error) {
	out, err := h.cs.LaboratoryV1alpha1().Labs(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	p := labToProto(out)
	fillLabUsage(p, h.namespaceUsage(ctx, in.Namespace))
	return p, nil
}

// ListLabs lists all Lab custom resources in the given namespace.
func (h *Handler) ListLabs(ctx context.Context, in *protobuf.NamespaceRequest) (*protobuf.LabList, error) {
	list, err := h.cs.LaboratoryV1alpha1().Labs(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	usage := h.namespaceUsage(ctx, in.Namespace)
	out := &protobuf.LabList{}
	for i := range list.Items {
		p := labToProto(&list.Items[i])
		fillLabUsage(p, usage)
		out.Items = append(out.Items, p)
	}
	return out, nil
}

// UpdateLab fetches the current Lab, applies the desired spec from in, and
// updates the resource.
func (h *Handler) UpdateLab(ctx context.Context, in *protobuf.Lab) (*protobuf.Lab, error) {
	cur, err := h.cs.LaboratoryV1alpha1().Labs(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	desired, err := protoToLab(in)
	if err != nil {
		return nil, err
	}
	cur.Spec = desired.Spec
	out, err := h.cs.LaboratoryV1alpha1().Labs(in.Namespace).Update(ctx, cur, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	if err := h.reconcileEnvSecrets(ctx, out, in.Env); err != nil {
		return nil, err
	}
	return labToProto(out), nil
}

// DeleteLab deletes a Lab custom resource by namespace and name.
func (h *Handler) DeleteLab(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.Empty, error) {
	if err := h.cs.LaboratoryV1alpha1().Labs(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}
