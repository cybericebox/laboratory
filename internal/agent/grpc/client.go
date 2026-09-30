package grpc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	kindLabGroupClient       = "LabGroupClient"
	kindLabGroupAccessPolicy = "LabGroupAccessPolicy"
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
	_, err = h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Create(ctx, lgc, metav1.CreateOptions{})
	if err := createErr(err, kindLabGroupClient, in.Name, func() (metav1.Object, error) {
		return h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	}); err != nil {
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

// DeleteLabGroupClient deletes a LabGroupClient custom resource by namespace and
// name. Deletion is asynchronous: it is finished once GetLabGroupClient returns
// NotFound; until then CreateLabGroupClient fails with Unavailable / TERMINATING.
func (h *Handler) DeleteLabGroupClient(ctx context.Context, in *protobuf.NamespacedIDRequest) (*protobuf.Empty, error) {
	if err := h.cs.LaboratoryV1alpha1().LabGroupClients(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}

// ReconcileLabGroupAccess replaces the one namespaced access-policy CR of a
// LabGroup. The VPN process watches that resource and applies its full
// default-deny rule set. Client names need not exist yet: the policy is stored
// at group scope and automatically applies once such a VPN client is created.
func (h *Handler) ReconcileLabGroupAccess(ctx context.Context, in *protobuf.LabGroupAccessPolicy) (*protobuf.Empty, error) {
	if in == nil || in.LabGroupName == "" {
		return nil, status.Error(codes.InvalidArgument, "lab_group_name is required")
	}
	group, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, in.LabGroupName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := rejectTerminating(kindLabGroup, group); err != nil {
		return nil, err
	}
	namespace := group.Status.Namespace
	if namespace == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "lab group %q namespace is not ready", in.LabGroupName)
	}

	labs, err := h.cs.LaboratoryV1alpha1().Labs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	knownLabs := make(map[string]struct{}, len(labs.Items))
	for i := range labs.Items {
		knownLabs[labs.Items[i].Name] = struct{}{}
	}
	rules := make([]laboratoryv1alpha1.LabGroupAccessRule, 0, len(in.Rules))
	for _, rule := range in.Rules {
		if rule == nil {
			return nil, status.Error(codes.InvalidArgument, "access rules cannot be null")
		}
		var action laboratoryv1alpha1.LabGroupAccessAction
		switch rule.Action {
		case protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW:
			action = laboratoryv1alpha1.LabGroupAccessAllow
		case protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY:
			action = laboratoryv1alpha1.LabGroupAccessDeny
		default:
			return nil, status.Error(codes.InvalidArgument, "every access rule requires allow or deny action")
		}
		labNames := uniqueSorted(rule.LabNames)
		for _, name := range labNames {
			if _, ok := knownLabs[name]; !ok {
				return nil, status.Errorf(codes.InvalidArgument, "lab %q does not belong to group %q", name, in.LabGroupName)
			}
		}
		rules = append(rules, laboratoryv1alpha1.LabGroupAccessRule{
			Action:      action,
			ClientNames: uniqueSorted(rule.ClientNames),
			LabNames:    labNames,
		})
	}
	policies := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace)
	stored, err := policies.Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = policies.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: namespace},
			Spec:       laboratoryv1alpha1.LabGroupAccessPolicySpec{Rules: rules},
		}, metav1.CreateOptions{})
	} else if err == nil {
		if err = rejectTerminating(kindLabGroupAccessPolicy, stored); err != nil {
			return nil, err
		}
		stored.Spec.Rules = rules
		_, err = policies.Update(ctx, stored, metav1.UpdateOptions{})
	}
	if err = createErr(err, kindLabGroupAccessPolicy, names.LabGroupAccessPolicyName, func() (metav1.Object, error) {
		return policies.Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
	}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}
	out := make([]string, 0, len(unique))
	for value := range unique {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
