package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

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
		if err := validateAccessFence(p); err != nil {
			return nil, invalid("policy %d (%s): %v", i, p.GetLabGroupName(), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
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
	group, err := resolver.get(ctx, in.LabGroupName)
	if err != nil {
		return protobuf.ItemState_ITEM_STATE_FAILED, err
	}
	if group.Annotations[names.AnnotationLifecycleRetirement] != "" {
		return protobuf.ItemState_ITEM_STATE_FAILED, fmt.Errorf("retired group cannot mutate access")
	}

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
	policyLabels = stampTenant(policyLabels, tenantOf(ctx))
	policyLabels, _ = mergeLabels(policyLabels, mergeItemLabels(common, in.Labels))

	rawMap, _ := json.Marshal(idMap)
	var annotations map[string]string
	if len(idMap) > 0 {
		annotations = map[string]string{names.AnnotationIDMap: string(rawMap)}
	}
	state := protobuf.ItemState_ITEM_STATE_EXISTS
	policiesAPI := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace)
	want := laboratoryv1alpha1.LabGroupAccessPolicySpec{Rules: rules, OperationID: in.OperationId, Revision: in.DesiredRevision, ExpectedGroupUID: in.ExpectedGroupUid}
	currentGroup := func() error {
		live, err := h.getGroup(ctx, in.LabGroupName)
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroup, live); err != nil {
			return err
		}
		if live.UID != group.UID || live.Status.Namespace != namespace || in.ExpectedGroupUid != "" && in.ExpectedGroupUid != string(live.UID) {
			return fmt.Errorf("access policy group identity changed")
		}
		if live.Annotations[names.AnnotationLifecycleRetirement] != "" {
			return fmt.Errorf("retired group cannot mutate access")
		}
		return nil
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		if err := currentGroup(); err != nil {
			return err
		}
		stored, err := policiesAPI.Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if in.PolicyUid != "" || in.Generation != 0 {
				return fmt.Errorf("expected access policy does not exist")
			}
			if err := currentGroup(); err != nil {
				return err
			}
			state = protobuf.ItemState_ITEM_STATE_CREATED
			_, err = policiesAPI.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: namespace, Labels: policyLabels, Annotations: copyLabels(annotations)},
				Spec:       want,
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(laboratoryv1alpha1.Resource("labgroupaccesspolicies"), names.LabGroupAccessPolicyName, err)
			}
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
		if !ownedBy(tenantOf(ctx), stored) {
			return notFoundForeign(kindLabGroupAccessPolicy, stored.Name)
		}
		if in.PolicyUid != "" && in.PolicyUid != string(stored.UID) {
			return fmt.Errorf("access policy UID or generation changed")
		}
		if accessSpecFenced(stored.Spec) {
			if !accessSpecFenced(want) || stored.Spec.ExpectedGroupUID != string(group.UID) {
				return fmt.Errorf("fenced access policy requires its current group identity")
			}
			if want.Revision < stored.Spec.Revision || want.Revision == stored.Spec.Revision && (want.OperationID != stored.Spec.OperationID || !rulesEqual(want.Rules, stored.Spec.Rules)) {
				return fmt.Errorf("access policy revision is stale or conflicts with the stored operation")
			}
		}
		labels, labelsChanged := mergeLabels(stored.Labels, policyLabels)
		if err := currentGroup(); err != nil {
			return err
		}
		specEqual := stored.Spec.OperationID == want.OperationID && stored.Spec.Revision == want.Revision && stored.Spec.ExpectedGroupUID == want.ExpectedGroupUID && rulesEqual(stored.Spec.Rules, want.Rules)
		if !labelsChanged && specEqual && stored.Annotations[names.AnnotationIDMap] == annotations[names.AnnotationIDMap] {
			return nil
		}
		if in.Generation != 0 && in.Generation != stored.Generation {
			return fmt.Errorf("access policy generation changed")
		}
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		stored.Spec = want
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

func accessSpecFenced(spec laboratoryv1alpha1.LabGroupAccessPolicySpec) bool {
	return spec.OperationID != "" || spec.Revision != 0 || spec.ExpectedGroupUID != ""
}
func validateAccessFence(in *protobuf.LabGroupAccessPolicy) error {
	if in.GetGeneration() < 0 {
		return fmt.Errorf("policy generation cannot be negative")
	}
	if in.GetOperationId() == "" && in.GetDesiredRevision() == 0 && in.GetExpectedGroupUid() == "" {
		return nil
	}
	if strings.TrimSpace(in.GetOperationId()) == "" || utf8.RuneCountInString(in.GetOperationId()) > 128 || in.GetDesiredRevision() < 1 || strings.TrimSpace(in.GetExpectedGroupUid()) == "" {
		return fmt.Errorf("operation_id, positive desired_revision and expected_group_uid are required together")
	}
	return nil
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
