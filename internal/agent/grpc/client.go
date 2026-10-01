package grpc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	kindLabGroupClient       = "LabGroupClient"
	kindLabGroupAccessPolicy = "LabGroupAccessPolicy"
)

func nsRef(group, name string) *protobuf.ItemRef {
	return &protobuf.ItemRef{LabGroup: group, Name: name}
}

// CreateLabGroupClients provisions WireGuard VPN clients. The agent generates every
// keypair: the PRIVATE key lives only in this call and never touches the cluster. Only
// the public key is registered; the controller assigns an IP and assembles the config
// (with a private-key placeholder) into the CR status. The agent then substitutes the real
// private key and returns the finished config. The caller (control plane) stores it; no
// later call returns it. A client that already exists is EXISTS (labels it lacks are
// added: UPDATED) and comes back with the placeholder config.
func (h *Handler) CreateLabGroupClients(ctx context.Context, in *protobuf.CreateLabGroupClientsRequest) (*protobuf.CreateLabGroupClientsResponse, error) {
	items := in.GetItems()
	if err := checkItemCount(len(items)); err != nil {
		return nil, err
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, invalid("%v", err)
	}
	refs := make([]*protobuf.ItemRef, len(items))
	for i, it := range items {
		refs[i] = nsRef(it.GetLabGroup(), it.GetName())
		if err := names.ValidateID(it.GetName()); err != nil {
			return nil, invalid("item %d: %v", i, err)
		}
		if err := validateLabels(it.GetLabels()); err != nil {
			return nil, invalid("item %d (%s): %v", i, describeRef(refs[i]), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	clients := make([]*protobuf.LabGroupClient, len(items))
	results := forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		res, c := h.createLabGroupClient(ctx, resolver, items[i], in.GetLabels())
		clients[i] = c
		return res
	})
	out := &protobuf.CreateLabGroupClientsResponse{Results: make([]*protobuf.LabGroupClientResult, len(items))}
	for i := range items {
		out.Results[i] = &protobuf.LabGroupClientResult{Result: results[i], Client: clients[i]}
	}
	return out, nil
}

func (h *Handler) createLabGroupClient(ctx context.Context, resolver *groupResolver, it *protobuf.LabGroupClientItem, common map[string]string) (*protobuf.ItemResult, *protobuf.LabGroupClient) {
	ref := nsRef(it.GetLabGroup(), it.GetName())
	ns, err := resolver.namespace(ctx, it.GetLabGroup())
	if err != nil {
		return failedResult(ref, err), nil
	}
	name := crName(it.GetName())
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return failedResult(ref, fmt.Errorf("generate wireguard key: %w", err)), nil
	}
	want := mergeItemLabels(common, it.GetLabels())
	clients := h.cs.LaboratoryV1alpha1().LabGroupClients(ns)
	lgc := &laboratoryv1alpha1.LabGroupClient{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: stampTenant(copyLabels(want), tenantOf(ctx)), Annotations: stampID(nil, it.GetName())},
	}
	lgc.Spec.PublicKey = priv.PublicKey().String()
	_, err = clients.Create(ctx, lgc, metav1.CreateOptions{})
	if err = createErr(err, kindLabGroupClient, it.GetName(), func() (metav1.Object, error) {
		return clients.Get(ctx, name, metav1.GetOptions{})
	}); apierrors.IsAlreadyExists(err) {
		return h.existingLabGroupClient(ctx, ref, ns, want)
	} else if err != nil {
		return failedResult(ref, err), nil
	}

	out, err := h.awaitClientConfig(ctx, ns, name)
	if err != nil {
		// The private key would be lost with this call, so the half-made client is
		// removed: the repetition (retryable, TERMINATING until it is gone) makes a new one.
		_ = clients.Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
		res := failedResult(ref, err)
		res.Retryable = true
		return res, nil
	}
	p := clientToProto(out)
	p.LabGroupName = it.GetLabGroup()
	if p.Status != nil && p.Status.Config != "" {
		p.Status.Config = strings.ReplaceAll(p.Status.Config, names.WGPrivateKeyPlaceholder, priv.String())
	}
	return result(ref, protobuf.ItemState_ITEM_STATE_CREATED), p
}

// existingLabGroupClient answers a create of a name that exists: adds missing labels.
func (h *Handler) existingLabGroupClient(ctx context.Context, ref *protobuf.ItemRef, ns string, want map[string]string) (*protobuf.ItemResult, *protobuf.LabGroupClient) {
	clients := h.cs.LaboratoryV1alpha1().LabGroupClients(ns)
	name := crName(ref.GetName())
	state := protobuf.ItemState_ITEM_STATE_EXISTS
	var cur *laboratoryv1alpha1.LabGroupClient
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		var err error
		if cur, err = clients.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroupClient, cur); err != nil {
			return err
		}
		if names.IDOf(cur) != ref.GetName() {
			return fmt.Errorf("%s %s: the name is taken by another id %q", kindLabGroupClient, ref.GetName(), names.IDOf(cur))
		}
		labels, changed := mergeLabels(cur.Labels, want)
		if !changed {
			return nil
		}
		cur.Labels = labels
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		cur, err = clients.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return failedResult(ref, err), nil
	}
	p := clientToProto(cur)
	p.LabGroupName = ref.GetLabGroup()
	return result(ref, state), p
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

// ListLabGroupClients lists LabGroupClients by names (items), by selector, or all of
// them; lab_group narrows the listing to one group. Their config carries the private-key
// placeholder (the cluster never holds the real key).
func (h *Handler) ListLabGroupClients(ctx context.Context, in *protobuf.ListRequest) (*protobuf.LabGroupClientList, error) {
	if err := checkListRequest(in); err != nil {
		return nil, err
	}
	out := &protobuf.LabGroupClientList{}
	if len(in.GetItems()) > 0 {
		resolver := h.newResolver(ctx)
		for _, ref := range in.GetItems() {
			g, err := resolver.get(ctx, ref.GetLabGroup())
			if apierrors.IsNotFound(err) || (err == nil && g.Status.Namespace == "") {
				continue
			}
			if err != nil {
				return nil, err
			}
			c, err := h.cs.LaboratoryV1alpha1().LabGroupClients(g.Status.Namespace).Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			p := clientToProto(c)
			p.LabGroupName = names.IDOf(g)
			out.Items = append(out.Items, p)
		}
		return out, nil
	}
	matches, err := h.listClients(ctx, in.GetSelector(), in.GetLabGroup())
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		p := clientToProto(m.client)
		p.LabGroupName = m.group
		out.Items = append(out.Items, p)
	}
	return out, nil
}

// UpdateLabGroupClients changes the labels of the targeted clients.
func (h *Handler) UpdateLabGroupClients(ctx context.Context, in *protobuf.UpdateLabGroupClientsRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var refs []*protobuf.ItemRef
	var plans []labelPlan
	if in.GetBySelector() != nil {
		matches, err := h.listClients(ctx, in.GetBySelector().GetSelector(), in.GetBySelector().GetLabGroup())
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(matches), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for _, m := range matches {
			refs = append(refs, nsRef(m.group, names.IDOf(m.client)))
		}
		sortRefs(refs)
		plan, err := mergeLabelChanges(in.GetLabels(), nil)
		if err != nil {
			return nil, invalid("%v", err)
		}
		for range refs {
			plans = append(plans, plan)
		}
	} else {
		for _, it := range in.GetItems() {
			ref := nsRef(it.GetLabGroup(), it.GetName())
			plan, err := mergeLabelChanges(in.GetLabels(), it.GetLabels())
			if err != nil {
				return nil, invalid("%s: %v", describeRef(ref), err)
			}
			refs, plans = append(refs, ref), append(plans, plan)
		}
		if err := dupRefs(refs); err != nil {
			return nil, err
		}
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		ns, err := resolver.namespace(ctx, refs[i].GetLabGroup())
		if err != nil {
			return failedResult(refs[i], err)
		}
		clients := h.cs.LaboratoryV1alpha1().LabGroupClients(ns)
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := clients.Get(ctx, crName(refs[i].GetName()), metav1.GetOptions{})
			if err != nil {
				return err
			}
			if err := rejectTerminating(kindLabGroupClient, cur); err != nil {
				return err
			}
			labels, changed := plans[i].apply(cur.Labels)
			if !changed {
				return nil
			}
			cur.Labels = labels
			_, err = clients.Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}

// DeleteLabGroupClients deletes LabGroupClients. Deletion is asynchronous: until it is
// finished, creating the same name fails with a retryable TERMINATING error.
func (h *Handler) DeleteLabGroupClients(ctx context.Context, in *protobuf.DeleteRequest) (*protobuf.BatchResult, error) {
	return h.deleteNamespaced(ctx, in, func(ctx context.Context, ns, name string) error {
		return h.cs.LaboratoryV1alpha1().LabGroupClients(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}, func(ctx context.Context, selector, labGroup string) ([]*protobuf.ItemRef, error) {
		matches, err := h.listClients(ctx, selector, labGroup)
		refs := make([]*protobuf.ItemRef, 0, len(matches))
		for _, m := range matches {
			refs = append(refs, nsRef(m.group, names.IDOf(m.client)))
		}
		return refs, err
	})
}

// deleteNamespaced is the Delete of a namespaced kind: refs by items or by selector,
// then one Delete call per ref in the namespace of its LabGroup.
func (h *Handler) deleteNamespaced(ctx context.Context, in *protobuf.DeleteRequest,
	del func(ctx context.Context, ns, name string) error,
	match func(ctx context.Context, selector, labGroup string) ([]*protobuf.ItemRef, error),
) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	refs := in.GetItems()
	if in.GetBySelector() != nil {
		var err error
		if refs, err = match(ctx, in.GetBySelector().GetSelector(), in.GetBySelector().GetLabGroup()); err != nil {
			return nil, err
		}
		if err := checkMatched(len(refs), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		sortRefs(refs)
	} else if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		g, err := resolver.get(ctx, refs[i].GetLabGroup())
		if err != nil {
			return failedResult(refs[i], err)
		}
		if g.Status.Namespace == "" {
			return result(refs[i], protobuf.ItemState_ITEM_STATE_NOT_FOUND)
		}
		return deleteResult(refs[i], del(ctx, g.Status.Namespace, crName(refs[i].GetName())))
	})}, nil
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
