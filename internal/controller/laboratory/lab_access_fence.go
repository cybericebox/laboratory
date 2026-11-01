package laboratory

import (
	"context"
	"reflect"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// accessFenceNoVPNPod stands for the boot of the VPN pod in the fence of a stop that found no VPN pod to fence.
const accessFenceNoVPNPod = "no-vpn-pod"

// fenceWithoutLivePod is the part of the fence that needs no VPN pod: the certificate of a stop that is already released, and a stop in a
// group that has no VPN pod to fence.
func (r *LabReconciler) fenceWithoutLivePod(ctx context.Context, l *lab.Lab) (bool, *metav1.Time, string, error) {
	// The stop fence is an immutable certificate after native release for the
	// same child UID/op/revision/generation. Group service pauses/restarts cannot
	// revive that child runtime. A child Start replaces lifecycle status and thus
	// clears this certificate before a new runtime is admitted.
	if stoppedAccessFenceCertified(l) {
		return true, l.Status.Lifecycle.AccessFencedAt, l.Status.Lifecycle.AccessFenceVPNBootID, nil
	}
	ok, at, err := r.noVPNPodFence(ctx, l)
	if err != nil || !ok {
		return false, nil, "", err
	}
	return true, at, accessFenceNoVPNPod, nil
}

// noVPNPodFence: a stop of a lab whose group is suspended on purpose (the VPN Deployment is there and at zero, and no VPN pod is left,
// read without the cache) has no access to fence: nothing can reach the lab through a pod that is not there. When the group is resumed the
// new VPN pod builds the access from the state of the day: a stopped lab is not ready, so no rule allows it (vpn/reconciler labAccessReady),
// and its network is cleared. A Deployment that is missing (a group not provisioned yet), a running one and a start all wait for the real
// fence, as before. The time of the fence stays the one of the first pass, so the lifecycle status does not change on every one.
func (r *LabReconciler) noVPNPodFence(ctx context.Context, l *lab.Lab) (bool, *metav1.Time, error) {
	if !l.Spec.Lifecycle.IsStopped() {
		return false, nil, nil
	}
	var dep appsv1.Deployment
	if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: names.ComponentVPN, Namespace: l.Namespace}, &dep); err != nil {
		return false, nil, client.IgnoreNotFound(err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		return false, nil, nil
	}
	absent, err := groupPodAbsent(ctx, r.lifecycleReader(), l.Namespace, names.ComponentVPN)
	if err != nil || !absent {
		return false, nil, err
	}
	if st := l.Status.Lifecycle; st != nil && st.AccessFenceVPNBootID == accessFenceNoVPNPod && nonzeroTime(st.AccessFencedAt) && st.OperationID == l.Spec.Lifecycle.OperationID && st.Revision == l.Spec.Lifecycle.Revision {
		return true, st.AccessFencedAt, nil
	}
	now := metav1.Now()
	return true, &now, nil
}

// currentAccessFence reads the native producer and current VPN Pod directly.
// API absence is unknown whenever this Lab configured VPN access.
func (r *LabReconciler) currentAccessFence(ctx context.Context, l *lab.Lab) (bool, *metav1.Time, string, error) {
	if !l.Spec.VPN.Enabled {
		return true, nil, "", nil
	}
	if ok, at, boot, err := r.fenceWithoutLivePod(ctx, l); err != nil || ok {
		return ok, at, boot, err
	}
	var leg lab.LabVPN
	if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: names.LabVPNObjectName(l.Name), Namespace: l.Namespace}, &leg); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, "", nil
		}
		return false, nil, "", err
	}
	f := leg.Status.AccessFence
	boot := leg.Status.Runtime
	if invalidAccessFenceIdentity(l, f, boot) {
		return false, nil, "", nil
	}
	owned := false
	for _, o := range leg.OwnerReferences {
		owned = owned || o.Kind == ownerKindLab && o.UID == l.UID
	}
	if !owned {
		return false, nil, "", nil
	}
	dr := &DeviceReconciler{Client: r.Client, Reader: r.lifecycleReader()}
	group, err := dr.labGroupOfNamespace(ctx, l.Namespace)
	if err != nil {
		return false, nil, "", err
	}
	if group == nil || f.GroupUID == "" || f.GroupUID != string(group.UID) {
		return false, nil, "", nil
	}
	var witness lab.LabTrafficReport
	if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: flowacct.ReportName, Namespace: l.Namespace}, &witness); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, "", nil
		}
		return false, nil, "", err
	}
	if invalidAccessFenceWitness(&witness, f, boot) {
		return false, nil, "", nil
	}
	var p corev1.Pod
	if err := r.lifecycleReader().Get(ctx, client.ObjectKey{Name: boot.PodName, Namespace: l.Namespace}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, "", nil
		}
		return false, nil, "", err
	}
	if !p.DeletionTimestamp.IsZero() || string(p.UID) != boot.PodUID || p.Labels[names.LabelComponent] != names.ComponentVPN || p.Status.Phase != corev1.PodRunning {
		return false, nil, "", nil
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == names.ComponentVPN && s.ContainerID != "" && s.ContainerID == boot.ContainerID && s.RestartCount == boot.RestartCount && s.State.Running != nil {
			return true, &f.FencedAt, f.BootID, nil
		}
	}
	return false, nil, "", nil
}

func stoppedAccessFenceCertified(l *lab.Lab) bool {
	return exactStoppedRelease(l) && nonzeroTime(l.Status.Resources.ObservedAt) && nonzeroTime(l.Status.Resources.ReleasedAt) && l.Status.Resources.AllocatedRequests == (lab.ResourceAmounts{}) && l.Status.Lifecycle.AccessFenced && nonzeroTime(l.Status.Lifecycle.AccessFencedAt) && l.Status.Lifecycle.AccessFenceVPNBootID != ""
}

func invalidAccessFenceIdentity(l *lab.Lab, f *lab.LabAccessFence, boot *lab.VPNRuntimeIdentity) bool {
	return f == nil || boot == nil || f.BootID == "" || !reflect.DeepEqual(*boot, f.VPNRuntimeIdentity) || f.LabUID != string(l.UID) || f.OperationID != l.Spec.Lifecycle.OperationID || f.Revision != l.Spec.Lifecycle.Revision || f.ObservedGeneration != l.Generation
}

func invalidAccessFenceWitness(witness *lab.LabTrafficReport, f *lab.LabAccessFence, boot *lab.VPNRuntimeIdentity) bool {
	return !witness.DeletionTimestamp.IsZero() || witness.Status.CurrentVPNRuntime == nil || witness.Status.CurrentVPNRuntime.PublishedAt.IsZero() || witness.Status.CurrentVPNRuntime.GroupUID != f.GroupUID || witness.Spec.Kind != lab.LabTrafficSurfaceVPN || witness.Spec.Instance != boot.PodName || !reflect.DeepEqual(witness.Status.CurrentVPNRuntime.VPNRuntimeIdentity, *boot)
}
