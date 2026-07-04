package grpc

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// wgConf reads the rendered WireGuard client config for clientName from its
// Secret (named names.SecretClientPrefix+clientName, data key "wg.conf").
// It is best-effort: the Secret may not exist yet (e.g. LabGroup endpoint
// not yet known), so any error is swallowed and nil is returned.
func (h *Handler) wgConf(ctx context.Context, ns, clientName string) []byte {
	s, err := h.k8s.CoreV1().Secrets(ns).Get(ctx, names.SecretClientPrefix+clientName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return s.Data["wg.conf"]
}

// CreateLabGroupClient creates a new LabGroupClient custom resource. The
// response carries the client's wg.conf if the Secret has already been
// reconciled (best-effort).
func (h *Handler) CreateLabGroupClient(ctx context.Context, in *protobuf.LabGroupClient) (*protobuf.LabGroupClient, error) {
	lgc := &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace}}
	lgc.Spec.PublicKey = in.PublicKey
	out, err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Create(ctx, lgc, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return clientToProto(out, h.wgConf(ctx, in.Namespace, in.Name)), nil
}

// GetLabGroupClient fetches a LabGroupClient custom resource by namespace
// and name, including its wg.conf read from the client Secret.
func (h *Handler) GetLabGroupClient(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.LabGroupClient, error) {
	out, err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return clientToProto(out, h.wgConf(ctx, in.Namespace, in.Name)), nil
}

// ListLabGroupClients lists all LabGroupClient custom resources in the
// given namespace. wg.conf is not populated for list responses.
func (h *Handler) ListLabGroupClients(ctx context.Context, in *protobuf.NamespaceRequest) (*protobuf.LabGroupClientList, error) {
	list, err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := &protobuf.LabGroupClientList{}
	for i := range list.Items {
		out.Items = append(out.Items, clientToProto(&list.Items[i], nil))
	}
	return out, nil
}

// DeleteLabGroupClient deletes a LabGroupClient custom resource by
// namespace and name.
func (h *Handler) DeleteLabGroupClient(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.Empty, error) {
	if err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}
