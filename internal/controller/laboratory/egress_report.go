package laboratory

import (
	"context"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labegressreports,verbs=get;list;watch;update;patch

// groupReports are the LabEgressReports the monitor believes: the object named "gateway" in a namespace that carries the group label (the
// namespaces of the operator's own LabGroups). A report of any other name or in any other namespace is passed over, and counted by reason.
// The reports are read from the informer cache of the manager (a watch), the namespaces from the API.
func (m *EgressMonitor) groupReports(ctx context.Context) ([]laboratoryv1alpha1.LabEgressReport, map[string]int, error) {
	skipped := map[string]int{}
	var nss corev1.NamespaceList
	if err := m.Reader.List(ctx, &nss, client.HasLabels{names.LabelGroup}); err != nil {
		return nil, skipped, err
	}
	groups := map[string]bool{}
	for i := range nss.Items {
		groups[nss.Items[i].Name] = true
	}
	var all laboratoryv1alpha1.LabEgressReportList
	if err := m.Client.List(ctx, &all); err != nil {
		return nil, skipped, err
	}
	var out []laboratoryv1alpha1.LabEgressReport
	for _, r := range all.Items {
		switch {
		case !groups[r.Namespace]:
			skipped["not-a-group-namespace"]++
		case r.Name != laboratoryv1alpha1.LabEgressReportName:
			skipped["not-named-gateway"]++
		default:
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out, skipped, nil
}

// ackTarget is a report and the newest minute of its namespace that the monitor has logged.
type ackTarget struct {
	report  *laboratoryv1alpha1.LabEgressReport
	through time.Time
}

// acknowledge writes spec.ackedThrough of a report: the newest minute of its namespace that the monitor has logged. It moves forward only
// and is written at most once per pull; a failed write is tried again by the next pull (the gateway keeps the minutes meanwhile).
func (m *EgressMonitor) acknowledge(ctx context.Context, r *laboratoryv1alpha1.LabEgressReport, through time.Time) error {
	if through.IsZero() || (r.Spec.AckedThrough != nil && !through.After(r.Spec.AckedThrough.Time)) {
		return nil
	}
	base := r.DeepCopy()
	t := metav1.NewTime(through.UTC())
	r.Spec.AckedThrough = &t
	return m.Client.Patch(ctx, r, client.MergeFrom(base))
}
