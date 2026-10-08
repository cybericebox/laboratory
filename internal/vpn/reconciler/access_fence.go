//go:build linux

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

func (r *AccessReconciler) direct() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}
func currentVPNRuntime(ctx context.Context, c client.Reader, namespace, podName, boot string) (lab.VPNRuntimeIdentity, error) {
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Name: podName, Namespace: namespace}, &p); err != nil {
		return lab.VPNRuntimeIdentity{}, err
	}
	if boot == "" || p.UID == "" || !p.DeletionTimestamp.IsZero() || p.Labels[names.LabelComponent] != names.ComponentVPN {
		return lab.VPNRuntimeIdentity{}, fmt.Errorf("current VPN pod identity is unknown")
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == names.ComponentVPN && s.ContainerID != "" && s.State.Running != nil {
			return lab.VPNRuntimeIdentity{BootID: boot, PodName: p.Name, PodUID: string(p.UID), ContainerID: s.ContainerID, RestartCount: s.RestartCount}, nil
		}
	}
	return lab.VPNRuntimeIdentity{}, fmt.Errorf("current VPN container identity is unknown")
}

// publishBoot runs before kernel work at startup, and for newly added legs.
// Raw patches touch only this owner's runtime/fence fields. The network owner
// must never remove them while updating DHCP/phase/conditions.
func (r *AccessReconciler) publishBoot(ctx context.Context, namespace string) error {
	current, err := currentVPNRuntime(ctx, r.direct(), namespace, r.Runtime.PodName, r.Runtime.BootID)
	if err != nil {
		return err
	}
	var witness lab.LabTrafficReport
	if err := r.direct().Get(ctx, client.ObjectKey{Name: flowacct.ReportName, Namespace: namespace}, &witness); err != nil {
		return err
	}
	record := witness.Status.CurrentVPNRuntime
	if !witness.DeletionTimestamp.IsZero() || witness.Spec.Kind != lab.LabTrafficSurfaceVPN || witness.Spec.Instance != r.Runtime.PodName || record == nil || record.PublishedAt.IsZero() || record.GroupUID != r.GroupUID || record.BootID != r.Runtime.BootID || record.PodUID != r.Runtime.PodUID {
		return fmt.Errorf("VPN process boot is superseded or unreported")
	}
	if !reflect.DeepEqual(current, r.Runtime) {
		// Kubelet may still report the previous container when this process starts.
		// Only the holder of the current startup boot may advance that same-Pod
		// metadata binding. Replacement Pods and backwards restart counters fail.
		if current.PodUID != r.Runtime.PodUID || current.PodName != r.Runtime.PodName || current.RestartCount < r.Runtime.RestartCount {
			return fmt.Errorf("VPN runtime changed owners since startup")
		}
		next := *record
		next.VPNRuntimeIdentity = current
		next.PublishedAt = metav1.Now()
		raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": witness.ResourceVersion}, "status": map[string]any{"currentVPNRuntime": next}})
		if err := r.Status().Patch(ctx, &witness, client.RawPatch(types.MergePatchType, raw)); err != nil {
			return err
		}
		r.Runtime = current
		r.applied = false
		r.initialChecked = false
		r.lastFenceKey = ""
	} else if !reflect.DeepEqual(record.VPNRuntimeIdentity, r.Runtime) {
		return fmt.Errorf("VPN startup metadata differs from current process binding")
	}
	var legs lab.LabVPNList
	if err := r.direct().List(ctx, &legs, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range legs.Items {
		leg := &legs.Items[i]
		if reflect.DeepEqual(leg.Status.Runtime, &r.Runtime) {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": leg.ResourceVersion}, "status": map[string]any{"runtime": r.Runtime, "accessFence": nil}})
		if err := r.Status().Patch(ctx, leg, client.RawPatch(types.MergePatchType, raw)); err != nil {
			return err
		}
	}
	return nil
}
func (r *AccessReconciler) writeStoppedFences(ctx context.Context, labs []lab.Lab) error {
	if r.Runtime.BootID == "" || r.GroupUID == "" || r.Conntrack == nil {
		return fmt.Errorf("physical fence requires current boot/group and conntrack retirement")
	}
	if err := r.publishBoot(ctx, r.lastNamespace); err != nil {
		return err
	}
	for _, old := range labs {
		if !old.Spec.VPN.Enabled || !old.Spec.Lifecycle.IsStopped() {
			continue
		}
		var current lab.Lab
		if err := r.direct().Get(ctx, client.ObjectKeyFromObject(&old), &current); err != nil {
			return err
		}
		if old.UID != current.UID || old.Generation != current.Generation || !reflect.DeepEqual(old.Spec.Lifecycle, current.Spec.Lifecycle) {
			return fmt.Errorf("lab changed during physical fence")
		}
		var leg lab.LabVPN
		if err := r.direct().Get(ctx, client.ObjectKey{Name: names.LabVPNObjectName(current.Name), Namespace: current.Namespace}, &leg); err != nil {
			return err
		}
		owned := false
		for _, o := range leg.OwnerReferences {
			owned = owned || o.Kind == "Lab" && o.UID == current.UID
		}
		if !owned {
			return fmt.Errorf("VPN leg ownership differs from live Lab")
		}
		fence := &lab.LabAccessFence{OperationID: current.Spec.Lifecycle.OperationID, Revision: current.Spec.Lifecycle.Revision, LabUID: string(current.UID), ObservedGeneration: current.Generation, GroupUID: r.GroupUID, VPNRuntimeIdentity: r.Runtime, FencedAt: metav1.Now()}
		if prev := leg.Status.AccessFence; prev != nil {
			a, b := *prev, *fence
			a.FencedAt = b.FencedAt
			if reflect.DeepEqual(a, b) {
				continue
			}
		}
		raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": leg.ResourceVersion}, "status": map[string]any{"accessFence": fence}})
		if err := r.Status().Patch(ctx, &leg, client.RawPatch(types.MergePatchType, raw)); err != nil {
			return err
		}
	}
	return nil
}
func stoppedFenceKey(labs []lab.Lab) string {
	type key struct {
		UID        string
		Generation int64
		Lifecycle  *lab.LabLifecycleSpec
	}
	var keys []key
	for _, l := range labs {
		if l.Spec.Lifecycle.IsStopped() {
			keys = append(keys, key{string(l.UID), l.Generation, l.Spec.Lifecycle})
		}
	}
	b, _ := json.Marshal(keys)
	return string(b)
}

// initializeCurrentBoot publishes the independent startup witness once, before
// any controller can acknowledge rules. Ordinary reconciles only consume it;
// an older process may not restore its boot over the new process's witness.
func (r *AccessReconciler) initializeCurrentBoot(ctx context.Context, namespace string) error {
	current, err := currentVPNRuntime(ctx, r.direct(), namespace, r.Runtime.PodName, r.Runtime.BootID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, r.Runtime) || r.GroupUID == "" {
		return fmt.Errorf("current VPN startup binding is unknown")
	}
	var report lab.LabTrafficReport
	key := client.ObjectKey{Name: flowacct.ReportName, Namespace: namespace}
	err = r.direct().Get(ctx, key, &report)
	if apierrors.IsNotFound(err) {
		report = lab.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Spec: lab.LabTrafficReportSpec{Kind: lab.LabTrafficSurfaceVPN, Instance: r.Runtime.PodName}}
		if err := r.Create(ctx, &report); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if report.Spec.Kind != lab.LabTrafficSurfaceVPN {
		return fmt.Errorf("VPN startup report has foreign kind")
	}
	if report.Spec.Instance != r.Runtime.PodName {
		report.Spec.Instance = r.Runtime.PodName
		if err := r.Update(ctx, &report); err != nil {
			return err
		}
	}
	record := lab.VPNBootRecord{VPNRuntimeIdentity: r.Runtime, GroupUID: r.GroupUID, PublishedAt: metav1.Now()}
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": report.ResourceVersion}, "status": map[string]any{"currentVPNRuntime": record}})
	return r.Status().Patch(ctx, &report, client.RawPatch(types.MergePatchType, raw))
}

// Kubelet status commonly trails process start. Keep the cold startup gate shut
// while the API has no running container identity, rather than crash-looping
// before kubelet ever reports it. A known prior tuple is safely rebound later.
func awaitVPNRuntime(ctx context.Context, c client.Reader, namespace, podName, boot string) (lab.VPNRuntimeIdentity, error) {
	attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		identity, err := currentVPNRuntime(attempt, c, namespace, podName, boot)
		if err == nil {
			return identity, nil
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return lab.VPNRuntimeIdentity{}, err
		}
		select {
		case <-attempt.Done():
			return lab.VPNRuntimeIdentity{}, fmt.Errorf("await current VPN container identity: %w", attempt.Err())
		case <-tick.C:
		}
	}
}
