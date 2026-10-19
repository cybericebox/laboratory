package laboratory

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=devices.laboratory.cybericebox.com;labgroups.laboratory.cybericebox.com,verbs=get

// gangCommitFields are the scheduling fields the gang commit writes; an API server serving an older CRD prunes them, and a
// withdrawal then deletes nothing and holds nothing back.
var gangCommitFields = []string{"withdrawals", "retryAfter", "onBudget"}

// GangCommitFieldsMissing reads the Device and LabGroup CRDs the API server serves and names the scheduling fields of the gang
// commit their schema lacks ("<crd>: <field>"); none: the CRDs are current. The manager reads it before it turns the capacity
// budget on: the chart's CRDs are not upgraded by helm upgrade.
func GangCommitFieldsMissing(ctx context.Context, r client.Reader) ([]string, error) {
	var missing []string
	for _, c := range []struct {
		name string
		path []string // from the status schema to the PodSchedule properties
	}{
		{"devices." + laboratoryv1alpha1.SchemeGroupVersion.Group, []string{"properties", "scheduling", "properties"}},
		{"labgroups." + laboratoryv1alpha1.SchemeGroupVersion.Group, []string{"properties", "pods", "items", "properties"}},
	} {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		if err := r.Get(ctx, client.ObjectKey{Name: c.name}, crd); err != nil {
			return nil, fmt.Errorf("read the CRD %s: %w", c.name, err)
		}
		props := servedStatusSchema(crd)
		for _, k := range c.path {
			next, _ := props[k].(map[string]any)
			props = next
		}
		for _, f := range gangCommitFields {
			if _, ok := props[f]; !ok {
				missing = append(missing, c.name+": "+f)
			}
		}
	}
	return missing, nil
}

// servedStatusSchema is the status schema of the served version of the operator's API in the CRD; nil when it has none.
func servedStatusSchema(crd *unstructured.Unstructured) map[string]any {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok || m["name"] != laboratoryv1alpha1.SchemeGroupVersion.Version {
			continue
		}
		status, _, _ := unstructured.NestedMap(m, "schema", "openAPIV3Schema", "properties", "status")
		return status
	}
	return nil
}

// BudgetWithCurrentCRDs is the capacity maximum to run with: m when it has no budget or the served CRDs know the fields of the
// gang commit; else none (the maximum is the current capacity), with an error in the log and a Warning event
// CapacityBudgetDisabled on the Device CRD, since a withdrawal against an older CRD would delete nothing and hold nothing
// back. Apply the chart's CRDs (helm upgrade does not) and restart the operator to turn the budget on.
func BudgetWithCurrentCRDs(ctx context.Context, r client.Reader, recorder record.EventRecorder, m nodecap.Maximum) nodecap.Maximum {
	if !m.Set() {
		return m
	}
	missing, err := GangCommitFieldsMissing(ctx, r)
	if err == nil && len(missing) == 0 {
		return m
	}
	why := fmt.Sprintf("the served CRDs lack %v", missing)
	if err != nil {
		why = err.Error()
	}
	log.FromContext(ctx).Error(err, "capacity budget disabled: apply the chart CRDs (kubectl apply -f charts/laboratory/crds/) and restart the operator", "missing", missing)
	if recorder != nil {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		crd.SetName("devices." + laboratoryv1alpha1.SchemeGroupVersion.Group)
		recorder.Eventf(crd, "Warning", "CapacityBudgetDisabled", "capacity.budget is not used: %s; apply the chart CRDs and restart the operator", why)
	}
	return nodecap.Maximum{}
}
