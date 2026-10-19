package laboratory

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/crdcheck"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=devices.laboratory.cybericebox.com;labgroups.laboratory.cybericebox.com,verbs=get

// CRDsFrom reads a CustomResourceDefinition through the reader (past any cache: the manager's API reader).
func CRDsFrom(r client.Reader) crdcheck.GetCRD {
	return func(ctx context.Context, name string) (map[string]any, error) {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		if err := r.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
			return nil, err
		}
		return crd.Object, nil
	}
}

// BudgetWithCurrentCRDs is the capacity maximum to run with (crdcheck.UsableBudget): with a budget and Device or LabGroup CRDs
// older than the gang commit, none (the maximum is the current capacity), with an error in the log and a Warning event
// CapacityBudgetDisabled on the Device CRD. Apply the chart's CRDs (`kubectl apply --server-side`; helm upgrade does not) and
// restart the operator to turn the budget on. The agent makes the same check, so it never advertises a budget the operator
// does not use.
func BudgetWithCurrentCRDs(ctx context.Context, r client.Reader, recorder record.EventRecorder, m nodecap.Maximum) nodecap.Maximum {
	got, why, kept := crdcheck.UsableBudget(ctx, CRDsFrom(r), m, crdcheck.BudgetRetry)
	if kept != nil {
		log.FromContext(ctx).Info("warning: CRD check failed, budget kept", "error", kept.Error())
	}
	if !m.Set() || got.Set() {
		return got
	}
	log.FromContext(ctx).Error(nil, "capacity budget disabled: apply the chart CRDs (kubectl apply --server-side -f charts/laboratory/crds/) and restart the operator", "why", why)
	if recorder != nil {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		crd.SetName("devices." + laboratoryv1alpha1.SchemeGroupVersion.Group)
		recorder.Eventf(crd, "Warning", "CapacityBudgetDisabled", "capacity.budget is not used: %s; apply the chart CRDs and restart the operator", why)
	}
	return got
}
