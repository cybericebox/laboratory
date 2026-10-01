package grpc

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/google/uuid"
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

// deployKey converts a deploy group (or a deploy_after entry) to a label-safe key
// of at most 63 characters: base36 of the UUID when it is one, otherwise "h" and
// the base36 of the SHA-256. Empty stays empty.
func deployKey(s string) string {
	if s == "" {
		return ""
	}
	if id, err := uuid.Parse(s); err == nil {
		return new(big.Int).SetBytes(id[:]).Text(36)
	}
	sum := sha256.Sum256([]byte(s))
	return "h" + new(big.Int).SetBytes(sum[:]).Text(36)
}

// deploySpec is the scheduling metadata of an object as the operator reads it:
// the deploy-group label value and the deploy-after annotation value.
type deploySpec struct {
	group string
	after string
}

func newDeploySpec(group string, after []string) deploySpec {
	var keys []string
	seen := map[string]bool{}
	for _, a := range after {
		k := deployKey(a)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return deploySpec{group: deployKey(group), after: strings.Join(keys, ",")}
}

// stamp writes the scheduling metadata into labels and annotations (allocating them).
func (d deploySpec) stamp(labels, annotations map[string]string) (map[string]string, map[string]string) {
	if d.group != "" {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[names.LabelDeployGroup] = d.group
	}
	if d.after != "" {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[names.AnnotationDeployAfter] = d.after
	}
	return labels, annotations
}

// matches reports whether an object already carries exactly this metadata.
func (d deploySpec) matches(labels, annotations map[string]string) bool {
	return labels[names.LabelDeployGroup] == d.group && annotations[names.AnnotationDeployAfter] == d.after
}

func invalid(format string, a ...any) error {
	return status.Errorf(codes.InvalidArgument, format, a...)
}
