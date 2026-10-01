package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const kindLab = "Lab"

// labVariant is a parsed LabVariant: the spec and the common variables, shared by every
// lab of the variant.
type labVariant struct {
	id   string
	spec laboratoryv1alpha1.LabSpec
	env  deviceVars
}

// parseVariants parses and validates the variants of a CreateLabs request. A variant id
// must be unique; a spec must be valid JSON of a Lab spec without device variables.
func parseVariants(in []*protobuf.LabVariant, persistence bool) (map[string]*labVariant, error) {
	out := make(map[string]*labVariant, len(in))
	for i, v := range in {
		if v.GetVariantId() == "" {
			return nil, invalid("variant %d: variant_id is required", i)
		}
		if _, dup := out[v.GetVariantId()]; dup {
			return nil, invalid("variant %q is sent twice", v.GetVariantId())
		}
		spec, err := parseLabSpec(v.GetSpecJson(), persistence)
		if err != nil {
			return nil, invalid("variant %q: %v", v.GetVariantId(), err)
		}
		env := mergeEnvLists(v.GetEnv())
		if err := validateEnv(env, specDevices(&spec)); err != nil {
			return nil, invalid("variant %q: %v", v.GetVariantId(), err)
		}
		out[v.GetVariantId()] = &labVariant{id: v.GetVariantId(), spec: spec, env: env}
	}
	return out, nil
}

// CreateLabs creates Lab custom resources from variants (the spec and common variables,
// sent once) and items. Re-sending an item whose lab exists with the same spec is
// EXISTS (the device Secrets are rewritten with the same values, labels it lacks are
// added: UPDATED); a lab with a different spec is FAILED for that item.
func (h *Handler) CreateLabs(ctx context.Context, in *protobuf.CreateLabsRequest) (*protobuf.BatchResult, error) {
	items := in.GetItems()
	if err := checkItemCount(len(items)); err != nil {
		return nil, err
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, invalid("%v", err)
	}
	variants, err := parseVariants(in.GetVariants(), h.statePersistence)
	if err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(items))
	envs := make([]deviceVars, len(items))
	for i, it := range items {
		refs[i] = nsRef(it.GetLabGroup(), it.GetName())
		if err := names.ValidateID(it.GetName()); err != nil {
			return nil, invalid("item %d: %v", i, err)
		}
		if err := names.ValidateID(it.GetLabGroup()); err != nil {
			return nil, invalid("item %d: lab_group: %v", i, err)
		}
		if _, err := newDeploySpec(it.GetDeployGroup(), it.GetDeployAfter()); err != nil {
			return nil, invalid("item %d (%s): %v", i, describeRef(refs[i]), err)
		}
		v, ok := variants[it.GetVariantId()]
		if !ok {
			return nil, invalid("item %d (%s): unknown variant_id %q", i, describeRef(refs[i]), it.GetVariantId())
		}
		if err := validateLabels(it.GetLabels()); err != nil {
			return nil, invalid("item %d (%s): %v", i, describeRef(refs[i]), err)
		}
		envs[i] = mergeEnvLists(variantEnvList(v), it.GetEnv())
		if err := validateEnv(envs[i], specDevices(&v.spec)); err != nil {
			return nil, invalid("item %d (%s): %v", i, describeRef(refs[i]), err)
		}
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		state, err := h.createLab(ctx, resolver, items[i], variants[items[i].GetVariantId()], envs[i], in.GetLabels())
		if err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], state)
	})}, nil
}

func variantEnvList(v *labVariant) []*protobuf.DeviceEnv {
	out := make([]*protobuf.DeviceEnv, 0, len(v.env))
	for _, dev := range sortedKeys(v.env) {
		out = append(out, &protobuf.DeviceEnv{Device: dev, Vars: v.env[dev]})
	}
	return out
}

// specHash fingerprints what an idempotent create compares: the spec and the scheduling
// metadata, as sent.
func specHash(spec *laboratoryv1alpha1.LabSpec, dep deploySpec) string {
	raw, _ := json.Marshal(struct {
		Spec  *laboratoryv1alpha1.LabSpec
		Group string
		After []string
	}{spec, dep.group, dep.after})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (h *Handler) createLab(ctx context.Context, resolver *groupResolver, it *protobuf.LabItem, v *labVariant, env deviceVars, common map[string]string) (protobuf.ItemState, error) {
	name := crName(it.GetName())
	ns, err := resolver.namespace(ctx, it.GetLabGroup())
	if err != nil {
		return 0, err
	}
	want := mergeItemLabels(common, it.GetLabels())
	dep, _ := newDeploySpec(it.GetDeployGroup(), it.GetDeployAfter())
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	v.spec.DeepCopyInto(&lab.Spec)
	lab.Labels, lab.Annotations = dep.stamp(stampTenant(copyLabels(want), tenantOf(ctx)), stampID(nil, it.GetName()))
	hash := specHash(&lab.Spec, dep)
	lab.Annotations[names.AnnotationSpecHash] = hash

	labs := h.cs.LaboratoryV1alpha1().Labs(ns)
	state := protobuf.ItemState_ITEM_STATE_CREATED
	out, err := labs.Create(ctx, lab, metav1.CreateOptions{})
	if err = createErr(err, kindLab, it.GetName(), func() (metav1.Object, error) {
		return labs.Get(ctx, name, metav1.GetOptions{})
	}); apierrors.IsAlreadyExists(err) {
		out, state, err = h.existingLab(ctx, ns, it.GetName(), hash, want)
	}
	if err != nil {
		return 0, err
	}

	devices := sortedKeys(env)
	if state != protobuf.ItemState_ITEM_STATE_CREATED {
		// An existing lab: every device ends with the Secret of this call (or none).
		devices = devices[:0]
		for i := range out.Spec.Devices {
			devices = append(devices, out.Spec.Devices[i].Name)
		}
	}
	if err := h.writeDeviceSecrets(ctx, out, devices, env); err != nil {
		return 0, err
	}
	return state, nil
}

// existingLab answers a create of a name that exists: the same spec (by hash) is fine,
// a different one is an error. Labels it lacks are added.
func (h *Handler) existingLab(ctx context.Context, ns, id, hash string, want map[string]string) (*laboratoryv1alpha1.Lab, protobuf.ItemState, error) {
	name := crName(id)
	labs := h.cs.LaboratoryV1alpha1().Labs(ns)
	state := protobuf.ItemState_ITEM_STATE_EXISTS
	var cur *laboratoryv1alpha1.Lab
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		state = protobuf.ItemState_ITEM_STATE_EXISTS
		var err error
		if cur, err = labs.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return err
		}
		if err := rejectTerminating(kindLab, cur); err != nil {
			return err
		}
		if names.IDOf(cur) != id {
			return fmt.Errorf("%s %s: the name is taken by another id %q", kindLab, id, names.IDOf(cur))
		}
		if cur.Annotations[names.AnnotationSpecHash] != hash {
			return errDifferentSpec{kindLab, id}
		}
		labels, changed := mergeLabels(cur.Labels, want)
		if !changed {
			return nil
		}
		cur.Labels = labels
		state = protobuf.ItemState_ITEM_STATE_UPDATED
		cur, err = labs.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	return cur, state, err
}

// ListLabs lists Labs by names (items), by selector, or all of them; lab_group narrows
// the listing to one LabGroup.
func (h *Handler) ListLabs(ctx context.Context, in *protobuf.ListRequest) (*protobuf.LabList, error) {
	if err := checkListRequest(in); err != nil {
		return nil, err
	}
	var matches []labMatch
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
			lab, err := h.cs.LaboratoryV1alpha1().Labs(g.Status.Namespace).Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			matches = append(matches, labMatch{group: names.IDOf(g), lab: lab})
		}
	} else {
		var err error
		if matches, err = h.listLabs(ctx, in.GetSelector(), in.GetLabGroup()); err != nil {
			return nil, err
		}
	}
	usage := map[string]map[usageKey]deviceUsage{}
	sched := map[string]map[usageKey]*laboratoryv1alpha1.PodSchedule{}
	out := &protobuf.LabList{}
	for _, m := range matches {
		u, ok := usage[m.lab.Namespace]
		if !ok {
			u = h.namespaceUsage(ctx, m.lab.Namespace)
			usage[m.lab.Namespace] = u
		}
		p := labToProto(m.lab)
		p.LabGroupName = m.group
		fillLabUsage(p, u, m.lab.Name)
		s, ok := sched[m.lab.Namespace]
		if !ok {
			s = h.namespaceDeviceScheduling(ctx, m.lab.Namespace)
			sched[m.lab.Namespace] = s
		}
		fillDeviceScheduling(p, s, m.lab.Name)
		out.Items = append(out.Items, p)
	}
	return out, nil
}

// labPlan is a validated set of changes for one lab.
type labPlan struct {
	labels labelPlan
	env    deviceVars
}

func planLabChanges(common, item *protobuf.LabChanges) (labPlan, error) {
	var p labPlan
	var err error
	if p.labels, err = mergeLabelChanges(common.GetLabels(), item.GetLabels()); err != nil {
		return p, err
	}
	p.env = mergeEnvLists(common.GetEnv(), item.GetEnv())
	return p, validateEnv(p.env, nil)
}

// UpdateLabs changes labels and rewrites device Secrets of the targeted labs. The
// spec of a lab does not change.
func (h *Handler) UpdateLabs(ctx context.Context, in *protobuf.UpdateLabsRequest) (*protobuf.BatchResult, error) {
	if err := checkSelection(in.GetBySelector().GetSelector(), len(in.GetItems()), in.GetBySelector() != nil); err != nil {
		return nil, err
	}
	var refs []*protobuf.ItemRef
	var plans []labPlan
	if in.GetBySelector() != nil {
		matches, err := h.listLabs(ctx, in.GetBySelector().GetSelector(), in.GetBySelector().GetLabGroup())
		if err != nil {
			return nil, err
		}
		if err := checkMatched(len(matches), in.GetBySelector().ExpectedCount); err != nil {
			return nil, err
		}
		for _, m := range matches {
			refs = append(refs, nsRef(m.group, names.IDOf(m.lab)))
		}
		sortRefs(refs)
		plan, err := planLabChanges(in.GetChanges(), nil)
		if err != nil {
			return nil, invalid("%v", err)
		}
		for range refs {
			plans = append(plans, plan)
		}
	} else {
		for _, it := range in.GetItems() {
			ref := nsRef(it.GetLabGroup(), it.GetName())
			plan, err := planLabChanges(in.GetChanges(), it.GetChanges())
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
		if err := h.updateLab(ctx, resolver, refs[i], plans[i]); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}

func (h *Handler) updateLab(ctx context.Context, resolver *groupResolver, ref *protobuf.ItemRef, p labPlan) error {
	ns, err := resolver.namespace(ctx, ref.GetLabGroup())
	if err != nil {
		return err
	}
	labs := h.cs.LaboratoryV1alpha1().Labs(ns)
	var cur *laboratoryv1alpha1.Lab
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		if cur, err = labs.Get(ctx, crName(ref.GetName()), metav1.GetOptions{}); err != nil {
			return err
		}
		if err := rejectTerminating(kindLab, cur); err != nil {
			return err
		}
		if len(p.env) > 0 {
			if err := validateEnv(p.env, specDevices(&cur.Spec)); err != nil {
				return invalid("%v", err)
			}
		}
		labels, changed := p.labels.apply(cur.Labels)
		if !changed {
			return nil
		}
		cur.Labels = labels
		cur, err = labs.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}
	if len(p.env) == 0 {
		return nil
	}
	return h.writeDeviceSecrets(ctx, cur, sortedKeys(p.env), p.env)
}

// DeleteLabs deletes Labs. Deletion is asynchronous: until it is finished, creating the
// same name fails with a retryable TERMINATING error.
func (h *Handler) DeleteLabs(ctx context.Context, in *protobuf.DeleteRequest) (*protobuf.BatchResult, error) {
	return h.deleteNamespaced(ctx, in, func(ctx context.Context, ns, name string) error {
		return h.cs.LaboratoryV1alpha1().Labs(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}, func(ctx context.Context, selector, labGroup string) ([]*protobuf.ItemRef, error) {
		matches, err := h.listLabs(ctx, selector, labGroup)
		refs := make([]*protobuf.ItemRef, 0, len(matches))
		for _, m := range matches {
			refs = append(refs, nsRef(m.group, names.IDOf(m.lab)))
		}
		return refs, err
	})
}
