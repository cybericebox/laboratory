package laboratory

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// StatePolicy is the device state persistence configuration of the operator.
// It is copied onto the Devices of a Lab when they are created and never
// re-applied, so changing it affects only labs created afterwards.
type StatePolicy struct {
	Enabled          bool
	Debounce         time.Duration
	ExcludePaths     []string
	MaxSnapshotBytes int64
	MaxLayers        int32
}

// ensureStateMode decides, once, whether the lab runs its devices as
// snapshot-backed Pods, and records it in Status.StatePersistence. A lab that
// already has Devices was created before the decision existed (or while the
// switch was off) and keeps running them as Deployments, so flipping the
// platform switch never changes a live lab. It reports whether it wrote the status.
func (r *LabReconciler) ensureStateMode(ctx context.Context, lab *laboratoryv1alpha1.Lab) (bool, error) {
	if lab.Status.StatePersistence != nil {
		return false, nil
	}
	enabled := r.State.Enabled
	if enabled {
		var devices laboratoryv1alpha1.DeviceList
		if err := r.List(ctx, &devices, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
			return false, err
		}
		if len(devices.Items) > 0 {
			enabled = false
		}
	}
	lab.Status.StatePersistence = &enabled
	if err := r.Status().Update(ctx, lab); err != nil {
		return false, err
	}
	return true, nil
}

// deviceStateSpec is the state policy of a new Device of the lab; nil when the
// lab does not use persistence or the device runs no container.
func (r *LabReconciler) deviceStateSpec(lab *laboratoryv1alpha1.Lab, t laboratoryv1alpha1.DeviceType) *laboratoryv1alpha1.DeviceStateSpec {
	if lab.Status.StatePersistence == nil || !*lab.Status.StatePersistence || t != laboratoryv1alpha1.DeviceTypeContainer {
		return nil
	}
	return &laboratoryv1alpha1.DeviceStateSpec{
		Enabled:          true,
		Debounce:         metav1.Duration{Duration: r.State.Debounce},
		ExcludePaths:     append([]string(nil), r.State.ExcludePaths...),
		MaxSnapshotBytes: r.State.MaxSnapshotBytes,
		MaxLayers:        r.State.MaxLayers,
	}
}

// deviceStateInfo is the organizer-facing summary of a device's snapshots.
func deviceStateInfo(d *laboratoryv1alpha1.Device) *laboratoryv1alpha1.DeviceStateInfo {
	if !deviceStateEnabled(d) || d.Status.State == nil {
		return nil
	}
	st := d.Status.State
	return &laboratoryv1alpha1.DeviceStateInfo{
		LastSnapshotAt: st.SnapshotAt,
		RestoredAt:     st.RestoredAt,
		SizeBytes:      st.SizeBytes,
		QuotaWarning:   st.Warning,
		Rescue:         st.Rescue,
	}
}

// snapshotInfoInterval is the least time between two writes of a device's
// snapshot time into the Lab status: snapshots can come every few seconds and
// every Lab status write reaches the monitoring stream.
const snapshotInfoInterval = 10 * time.Second

// throttleStateInfo keeps the previously published snapshot time while the new
// one is less than snapshotInfoInterval later and nothing else changed.
func throttleStateInfo(old, cur []laboratoryv1alpha1.DeviceRef) (throttled bool) {
	prev := map[string]*laboratoryv1alpha1.DeviceStateInfo{}
	for i := range old {
		prev[old[i].Name] = old[i].State
	}
	for i := range cur {
		p, n := prev[cur[i].Name], cur[i].State
		if p == nil || n == nil || p.LastSnapshotAt == nil || n.LastSnapshotAt == nil {
			continue
		}
		if d := n.LastSnapshotAt.Sub(p.LastSnapshotAt.Time); d > 0 && d < snapshotInfoInterval {
			c := *n
			c.LastSnapshotAt = p.LastSnapshotAt
			cur[i].State = &c
			throttled = true
		}
	}
	return throttled
}
