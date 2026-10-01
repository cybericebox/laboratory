package grpc

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// copyLabels returns a copy of a label map (nil for none).
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// mergeLabels adds the given labels to dst (allocating it when needed) and
// reports whether anything changed. A label is never removed this way.
func mergeLabels(dst map[string]string, add map[string]string) (map[string]string, bool) {
	changed := false
	for k, v := range add {
		if cur, ok := dst[k]; ok && cur == v {
			continue
		}
		if dst == nil {
			dst = map[string]string{}
		}
		dst[k] = v
		changed = true
	}
	return dst, changed
}

// validateLabels checks user labels: valid Kubernetes label syntax and not the
// platform's reserved prefix.
func validateLabels(in map[string]string) error {
	for k, v := range in {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Errorf("label %q: invalid value %q: %s", k, v, strings.Join(errs, "; "))
		}
	}
	return nil
}

func validateLabelKey(k string) error {
	if strings.HasPrefix(k, names.LabelPrefix) {
		return fmt.Errorf("label %q: the prefix %s is reserved", k, names.LabelPrefix)
	}
	if errs := validation.IsQualifiedName(k); len(errs) > 0 {
		return fmt.Errorf("label key %q: %s", k, strings.Join(errs, "; "))
	}
	return nil
}

// mergeItemLabels is the request-level labels overlaid by the item's own (the
// item wins on a conflict).
func mergeItemLabels(common, item map[string]string) map[string]string {
	if len(common) == 0 && len(item) == 0 {
		return nil
	}
	out := make(map[string]string, len(common)+len(item))
	for k, v := range common {
		out[k] = v
	}
	for k, v := range item {
		out[k] = v
	}
	return out
}

// labelPlan is a validated label change: labels to set and keys to remove.
type labelPlan struct {
	set    map[string]string
	remove []string
}

// mergeLabelChanges overlays the item's changes on the request-level ones: set
// maps merge (the item wins), remove lists unite. A key both set and removed is
// invalid, as is any reserved or malformed label.
func mergeLabelChanges(common, item *protobuf.LabelChanges) (labelPlan, error) {
	var plan labelPlan
	plan.set = mergeItemLabels(common.GetSet(), item.GetSet())
	seen := map[string]bool{}
	for _, list := range [][]string{common.GetRemove(), item.GetRemove()} {
		for _, k := range list {
			if !seen[k] {
				seen[k] = true
				plan.remove = append(plan.remove, k)
			}
		}
	}
	sort.Strings(plan.remove)
	if err := validateLabels(plan.set); err != nil {
		return plan, err
	}
	for _, k := range plan.remove {
		if err := validateLabelKey(k); err != nil {
			return plan, err
		}
		if _, ok := plan.set[k]; ok {
			return plan, fmt.Errorf("label %q is both set and removed", k)
		}
	}
	return plan, nil
}

// apply returns the labels after the plan and whether they changed.
func (p labelPlan) apply(cur map[string]string) (map[string]string, bool) {
	out := copyLabels(cur)
	changed := false
	for _, k := range p.remove {
		if _, ok := out[k]; ok {
			delete(out, k)
			changed = true
		}
	}
	var c bool
	out, c = mergeLabels(out, p.set)
	return out, changed || c
}

// deploySpec is the scheduling metadata of an object: the original deploy group and
// deploy_after keys. On the object the group is the reserved label (names.DeployKey of
// it) and the annotations hold the originals.
type deploySpec struct {
	group string
	after []string
}

// newDeploySpec validates the scheduling fields: a group of at most 64 characters, at
// most 32 deploy_after keys of at most 64 characters each, none with a comma (the
// annotation is comma-separated). Duplicates and empty keys are dropped.
func newDeploySpec(group string, after []string) (deploySpec, error) {
	d := deploySpec{group: group}
	if err := checkDeployKey(group); err != nil {
		return d, fmt.Errorf("deploy_group: %w", err)
	}
	if len(after) > names.MaxDeployAfter {
		return d, fmt.Errorf("deploy_after has %d keys, at most %d", len(after), names.MaxDeployAfter)
	}
	seen := map[string]bool{}
	for _, a := range after {
		if err := checkDeployKey(a); err != nil {
			return d, fmt.Errorf("deploy_after: %w", err)
		}
		if a != "" && !seen[a] {
			seen[a] = true
			d.after = append(d.after, a)
		}
	}
	return d, nil
}

func checkDeployKey(k string) error {
	if len(k) > names.MaxIDLen {
		return fmt.Errorf("%q is longer than %d characters", k, names.MaxIDLen)
	}
	if strings.Contains(k, ",") {
		return fmt.Errorf("%q has a comma", k)
	}
	return nil
}

// stamp writes the scheduling metadata into labels and annotations (allocating them).
func (d deploySpec) stamp(labels, annotations map[string]string) (map[string]string, map[string]string) {
	if d.group != "" {
		if labels == nil {
			labels = map[string]string{}
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		labels[names.LabelDeployGroup] = names.DeployKey(d.group)
		annotations[names.AnnotationDeployGroup] = d.group
	}
	if len(d.after) > 0 {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[names.AnnotationDeployAfter] = strings.Join(d.after, ",")
	}
	return labels, annotations
}

// matches reports whether an object already carries exactly this metadata.
func (d deploySpec) matches(labels, annotations map[string]string) bool {
	return labels[names.LabelDeployGroup] == names.DeployKey(d.group) &&
		annotations[names.AnnotationDeployGroup] == d.group &&
		annotations[names.AnnotationDeployAfter] == strings.Join(d.after, ",")
}

// deployOf reads the original scheduling fields of an object back.
func deployOf(annotations map[string]string) (string, []string) {
	var after []string
	if v := annotations[names.AnnotationDeployAfter]; v != "" {
		after = strings.Split(v, ",")
	}
	return annotations[names.AnnotationDeployGroup], after
}

// crName is the CR name of a client-supplied id.
func crName(id string) string { return names.EncodeName(id) }

// stampID records the original id on an object.
func stampID(annotations map[string]string, id string) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[names.AnnotationID] = id
	return annotations
}

func invalid(format string, a ...any) error {
	return status.Errorf(codes.InvalidArgument, format, a...)
}
