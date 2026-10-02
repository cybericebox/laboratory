package grpc

import (
	"context"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Maintenance window states.
const (
	windowUpcoming = "Upcoming"
	windowActive   = "Active"
	windowPast     = "Past"
)

// windowState says where a window is relative to now.
func windowState(w *laboratoryv1alpha1.MaintenanceWindow, now time.Time) string {
	switch {
	case now.Before(w.Spec.From.Time):
		return windowUpcoming
	case w.Spec.To != nil && !now.Before(w.Spec.To.Time):
		return windowPast
	}
	return windowActive
}

// windowAppliesTo reports whether a window applies to a tenant: one with no tenants applies
// to all. A listed tenant is matched the way the caller's tenant is derived (a certificate CN
// longer than a label value is hashed), so the spec can name the CN as it is.
func windowAppliesTo(w *laboratoryv1alpha1.MaintenanceWindow, tenantName string) bool {
	if len(w.Spec.Tenants) == 0 {
		return true
	}
	for _, t := range w.Spec.Tenants {
		if names.TenantKey(t) == tenantName {
			return true
		}
	}
	return false
}

// ListMaintenanceWindows lists the maintenance windows announced by the cluster operator that
// apply to the caller's tenant, soonest first. Windows that are over are left out unless asked
// for. The agent itself refuses and delays nothing during a window: the backend plans around it.
func (h *Handler) ListMaintenanceWindows(ctx context.Context, in *protobuf.ListMaintenanceWindowsRequest) (*protobuf.MaintenanceWindowList, error) {
	list, err := h.cs.LaboratoryV1alpha1().MaintenanceWindows().List(ctx, metav1.ListOptions{})
	if err != nil {
		if isNotFound(err) {
			// The CRD is not installed (the crds/ folder is not upgraded by helm): not an empty list, so the backend does not
			// take "no window" for the cluster's word.
			return nil, status.Error(codes.FailedPrecondition, "the MaintenanceWindow CRD is not installed: kubectl apply --server-side -f charts/laboratory/crds/")
		}
		return nil, err
	}
	tenantName := tenantOf(ctx)
	now := time.Now()
	var items []laboratoryv1alpha1.MaintenanceWindow
	for i := range list.Items {
		w := &list.Items[i]
		if !windowAppliesTo(w, tenantName) || (windowState(w, now) == windowPast && !in.GetIncludePast()) {
			continue
		}
		items = append(items, *w)
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].Spec.From.Time, items[j].Spec.From.Time
		if !a.Equal(b) {
			return a.Before(b)
		}
		return items[i].Name < items[j].Name
	})
	out := &protobuf.MaintenanceWindowList{}
	for i := range items {
		w := &items[i]
		p := &protobuf.MaintenanceWindow{
			Name: w.Name, FromUnixMs: w.Spec.From.UnixMilli(), Reason: w.Spec.Reason,
			State: windowState(w, now), AllTenants: len(w.Spec.Tenants) == 0,
		}
		if len(w.Spec.Capacity) > 0 {
			p.HasCapacity = true
			p.CapacityCpuMillicores = w.Spec.Capacity.Cpu().MilliValue()
			p.CapacityMemoryBytes = w.Spec.Capacity.Memory().Value()
		}
		if w.Spec.To != nil {
			p.ToUnixMs = w.Spec.To.UnixMilli()
		}
		out.Items = append(out.Items, p)
	}
	return out, nil
}
