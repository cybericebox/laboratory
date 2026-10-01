package grpc

import (
	"context"
	"encoding/json"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// SetLabGroupAccess replaces the one namespaced access-policy CR of every listed
// LabGroup. The VPN process watches that resource and applies its full default-deny
// rule set. Client names need not exist yet: the policy is stored at group scope and
// automatically applies once such a VPN client is created. Lab names must be labs of the
// group. Result per policy: CREATED, UPDATED, or EXISTS when nothing changed.
func (h *Handler) SetLabGroupAccess(ctx context.Context, in *protobuf.SetLabGroupAccessRequest) (*protobuf.BatchResult, error) {
	policies := in.GetPolicies()
	if err := checkItemCount(len(policies)); err != nil {
		return nil, err
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, invalid("%v", err)
	}
	refs := make([]*protobuf.ItemRef, len(policies))
	for i, p := range policies {
		refs[i] = groupRef(p.GetLabGroupName())
		if err := names.ValidateID(p.GetLabGroupName()); err != nil {
			return nil, invalid("policy %d: lab_group_name: %v", i, err)
		}
		if err := validateLabels(p.GetLabels()); err != nil {
			return nil, invalid("policy %d (%s): %v", i, p.GetLabGroupName(), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver()
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		state, err := h.setLabGroupAccess(ctx, resolver, policies[i], in.GetLabels())
		if err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], state)
	})}, nil
}

func (h *Handler) setLabGroupAccess(ctx context.Context, resolver *groupResolver, in *protobuf.LabGroupAccessPolicy, common map[string]string) (protobuf.ItemState, error) {
	namespace, err := resolver.namespace(ctx, in.LabGroupName)
	if err != nil {
		return 0, err
	}
	group, _ := resolver.get(ctx, in.LabGroupName)

	labs, err := h.cs.LaboratoryV1alpha1().Labs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	knownLabs := make(map[string]struct{}, len(labs.Items))
	for i := range labs.Items {
		knownLabs[labs.Items[i].Name] = struct{}{}
	}
	idMap := map[string]string{}
	rules := make([]laboratoryv1alpha1.LabGroupAccessRule, 0, len(in.Rules))
	for _, rule := range in.Rules {
		if rule == nil {
			return 0, invalid("access rules cannot be null")
		}
		var action laboratoryv1alpha1.LabGroupAccessAction
		switch rule.Action {
		case protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW:
			action = laboratoryv1alpha1.LabGroupAccessAllow
		case protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY:
			action = laboratoryv1alpha1.LabGroupAccessDeny
		default:
			return 0, invalid("every access rule requires allow or deny action")
		}
		labNames := uniqueSorted(rule.LabNames)
		for _, id := range labNames {
			if _, ok := knownLabs[crName(id)]; !ok {
				return 0, invalid("lab %q does not belong to group %q", id, in.LabGroupName)
			}
		}
		// The VPN reconciler matches CR names; the originals go to the id map.
		rules = append(rules, laboratoryv1alpha1.LabGroupAccessRule{
			Action:      action,
			ClientNames: encodeNames(uniqueSorted(rule.ClientNames), idMap),
			LabNames:    encodeNames(labNames, idMap),
		})
	}
	// The policy carries the labels of its group (plus the given ones), so a selector
	// that matches the group matches its policy too. The group's scheduling label is
	// the group's own.
	policyLabels := copyLabels(group.Labels)
	delete(policyLabels, names.LabelDeployGroup)
	policyLabels, _ = mergeLabels(policyLabels, mergeItemLabels(common, in.Labels))

	rawMap, _ := json.Marshal(idMap)
	var annotations map[string]string
	if len(idMap) > 0 {
		annotations = map[string]string{names.AnnotationIDMap: string(rawMap)}
	}
	state := protobuf.ItemState_ITEM_STATE_EXISTS
	policiesAPI := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		stored, err := policiesAPI.Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			state = protobuf.ItemState_ITEM_STATE_CREATED
			_, err = policiesAPI.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: namespace, Labels: policyLabels, Annotations: copyLabels(annotations)},
				Spec:       laboratoryv1alpha1.LabGroupAccessPolicySpec{Rules: rules},
			}, metav1.CreateOptions{})
			return createErr(err, kindLabGroupAccessPolicy, names.LabGroupAccessPolicyName, func() (metav1.Object, error) {
				return policiesAPI.Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
			})
		}
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroupAccessPolicy, stored); err != nil {
			return err
		}
		labels, labelsChanged := mergeLabels(stored.Labels, policyLabels)
		if !labelsChanged && rulesEqual(stored.Spec.Rules, rules) && stored.Annotations[names.AnnotationIDMap] == annotations[names.AnnotationIDMap] {
			return nil
		}
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		stored.Spec.Rules = rules
		stored.Labels = labels
		stored.Annotations = copyLabels(stored.Annotations)
		if len(idMap) > 0 {
			stored.Annotations = stampAnn(stored.Annotations, names.AnnotationIDMap, string(rawMap))
		} else {
			delete(stored.Annotations, names.AnnotationIDMap)
		}
		_, err = policiesAPI.Update(ctx, stored, metav1.UpdateOptions{})
		return err
	})
	return state, err
}

func rulesEqual(a, b []laboratoryv1alpha1.LabGroupAccessRule) bool {
	return len(a) == len(b) && (len(a) == 0 || apiequality.Semantic.DeepEqual(a, b))
}

func stampAnn(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

// encodeNames maps ids to CR names, recording the ids whose name differs.
func encodeNames(ids []string, idMap map[string]string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = crName(id)
		if out[i] != id {
			idMap[out[i]] = id
		}
	}
	return out
}
