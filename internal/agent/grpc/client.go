package grpc

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// CreateLabGroupClient provisions a WireGuard VPN client. The agent generates
// the keypair — the PRIVATE key lives only in this call and never touches the
// cluster. Only the public key is registered; the controller assigns an IP and
// assembles the config (with a private-key placeholder) into the CR status. The
// agent then substitutes the real private key and returns the finished config.
// The caller (control plane) stores it; a later Get returns only the
// placeholder config, since the private key is gone.
func (h *Handler) CreateLabGroupClient(ctx context.Context, in *protobuf.LabGroupClient) (*protobuf.LabGroupClient, error) {
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate wireguard key: %w", err)
	}

	lgc := &laboratoryv1alpha1.LabGroupClient{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace},
	}
	lgc.Spec.PublicKey = priv.PublicKey().String()
	if _, err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Create(ctx, lgc, metav1.CreateOptions{}); err != nil {
		return nil, err
	}

	out, err := h.awaitClientConfig(ctx, in.Namespace, in.Name)
	if err != nil {
		return nil, err
	}

	p := clientToProto(out)
	if p.Status != nil && p.Status.Config != "" {
		p.Status.Config = strings.ReplaceAll(p.Status.Config, names.WGPrivateKeyPlaceholder, priv.String())
	}
	return p, nil
}

// awaitClientConfig polls the LabGroupClient until the reconciler has assembled
// its config into status (it needs an assigned IP plus the parent LabGroup VPN
// endpoint + server public key).
func (h *Handler) awaitClientConfig(ctx context.Context, ns, name string) (*laboratoryv1alpha1.LabGroupClient, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		out, err := h.cs.LaboratoryV1alpha1().LabGroupClients(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if out.Status.Config != "" {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for VPN client %q config", name)
		case <-ticker.C:
		}
	}
}

// GetLabGroupClient fetches a LabGroupClient. Its config carries the private-key
// placeholder (the cluster never holds the real key) — callers rely on the
// config returned by CreateLabGroupClient.
func (h *Handler) GetLabGroupClient(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.LabGroupClient, error) {
	out, err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return clientToProto(out), nil
}

// ListLabGroupClients lists all LabGroupClient custom resources in the namespace.
func (h *Handler) ListLabGroupClients(ctx context.Context, in *protobuf.NamespaceRequest) (*protobuf.LabGroupClientList, error) {
	list, err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := &protobuf.LabGroupClientList{}
	for i := range list.Items {
		out.Items = append(out.Items, clientToProto(&list.Items[i]))
	}
	return out, nil
}

// DeleteLabGroupClient deletes a LabGroupClient custom resource by namespace and name.
func (h *Handler) DeleteLabGroupClient(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.Empty, error) {
	if err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}
