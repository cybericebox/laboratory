package laboratory

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/tenant"
)

// StatePolicy is the device state persistence configuration of the operator.
// It is copied onto the Devices of a Lab when they are created and never
// re-applied, so changing it affects only labs created afterwards.
type StatePolicy struct {
	Enabled         bool
	Debounce        time.Duration
	ExcludePaths    []string
	WriteQuotaBytes int64
	MaxFileBytes    int64
	MaxEntries      int32
	TenantQuota     int64
	MaxLayers       int32
}

// ensureModes decides, once, whether the lab's images are pulled through the image
// cache (Status.ImageCache). A lab that already has Devices was created before the
// decision existed (or while the switch was off) and keeps its mode, so flipping a
// platform switch never changes a live lab. State persistence is not a lab mode: each
// device decides at its creation (deviceStateSpec). It reports whether it wrote the status.
func (r *LabReconciler) ensureModes(ctx context.Context, lab *laboratoryv1alpha1.Lab) (bool, error) {
	if lab.Status.ImageCache != nil {
		return false, nil
	}
	cache := r.Mirror.Prefix != ""
	if cache {
		// A tenant with registry credentials of its own pulls straight from its registries:
		// the shared cache would fetch its private images with the platform's credentials.
		secret, err := tenantPullSecret(ctx, r, names.TenantOf(lab.Labels))
		if err != nil {
			return false, err
		}
		cache = secret == ""
	}
	if cache {
		var devices laboratoryv1alpha1.DeviceList
		if err := r.List(ctx, &devices, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
			return false, err
		}
		if len(devices.Items) > 0 {
			cache = false
		}
	}
	lab.Status.ImageCache = &cache
	if cache {
		r.pinImages(ctx, lab)
	}
	if err := r.Status().Update(ctx, lab); err != nil {
		return false, err
	}
	return true, nil
}

// deviceMirror is the image cache prefix of a new Device of the lab; empty when
// the lab does not use the cache or the device runs no container.
func (r *LabReconciler) deviceMirror(lab *laboratoryv1alpha1.Lab, t laboratoryv1alpha1.DeviceType) string {
	if lab.Status.ImageCache == nil || !*lab.Status.ImageCache || t != laboratoryv1alpha1.DeviceTypeContainer {
		return ""
	}
	return r.Mirror.Prefix
}

// tenantOf is the Tenant a Lab belongs to (by its tenant label; the default tenant without
// one); nil when there is no such Tenant object, which then gets the platform's policy.
func (r *LabReconciler) tenantOf(ctx context.Context, lab *laboratoryv1alpha1.Lab) (*laboratoryv1alpha1.Tenant, error) {
	var t laboratoryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: names.TenantOf(lab.Labels)}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// deviceStateSpec is the state policy of a new Device, decided once at its creation: the
// topology asked for persistence (devices[].persistence.enabled), the platform and the tenant allow it
// (its write quota and file size limit are the tenant's, capped by the platform's) and the device runs a container. Otherwise nil: a Deployment. The Device keeps what was
// stamped, so later changes of the platform switch never change an existing device. One lab
// may mix both kinds.
func (r *LabReconciler) deviceStateSpec(ten *laboratoryv1alpha1.Tenant, tmpl laboratoryv1alpha1.DeviceTemplate) *laboratoryv1alpha1.DeviceStateSpec {
	pers := tenant.EffectivePersistence(ten, r.State.Enabled, r.State.WriteQuotaBytes, r.State.MaxFileBytes, r.State.TenantQuota)
	if !pers.Allowed || tmpl.Persistence == nil || !tmpl.Persistence.Enabled || tmpl.Type != laboratoryv1alpha1.DeviceTypeContainer {
		return nil
	}
	// The topology may set the debounce of a device; the excluded paths and the quota are the platform's.
	debounce := metav1.Duration{Duration: r.State.Debounce}
	if p := tmpl.Persistence; p.Debounce != nil {
		debounce = *p.Debounce
	}
	return &laboratoryv1alpha1.DeviceStateSpec{
		Enabled:          true,
		Debounce:         debounce,
		ExcludePaths:     append([]string(nil), r.State.ExcludePaths...),
		WriteQuotaBytes:  pers.WriteQuota,
		MaxFileBytes:     pers.MaxFileSize,
		MaxEntries:       r.State.MaxEntries,
		TenantQuotaBytes: pers.RegistryQuota,
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

// pinImages resolves the image tags of the lab's container devices, and the
// netconfig image, to digests once, at creation, and records them in the Lab
// status: the whole lab pulls exactly that content. An image that cannot be
// resolved is left to its tag and named in Status.ImageWarning.
func (r *LabReconciler) pinImages(ctx context.Context, lab *laboratoryv1alpha1.Lab) {
	if r.Resolver == nil {
		return
	}
	var refs []string
	seen := map[string]bool{}
	add := func(ref string) {
		if ref != "" && !seen[ref] && r.Mirror.Rewrite(ref) != ref {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	for _, d := range lab.Spec.Devices {
		if d.Type == laboratoryv1alpha1.DeviceTypeContainer {
			add(d.Image)
		}
	}
	if len(refs) > 0 {
		add(r.NetConfigImage)
	}
	digests := map[string]string{}
	var failed []string
	for _, ref := range refs {
		d, err := r.Resolver.Resolve(ctx, ref)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", ref, err))
			continue
		}
		digests[ref] = d
	}
	if len(digests) > 0 {
		lab.Status.ImageDigests = digests
	}
	if len(failed) > 0 {
		lab.Status.ImageWarning = "image tags not pinned to a digest, pulled by tag: " + strings.Join(failed, "; ")
		if r.Recorder != nil {
			r.Recorder.Event(lab, corev1.EventTypeWarning, "ImageNotPinned", lab.Status.ImageWarning)
		}
	}
}

// deviceDigests is the pinned digests of a new Device's images: its own image
// and the netconfig image.
func (r *LabReconciler) deviceDigests(lab *laboratoryv1alpha1.Lab, tmpl laboratoryv1alpha1.DeviceTemplate) map[string]string {
	if r.deviceMirror(lab, tmpl.Type) == "" {
		return nil
	}
	out := map[string]string{}
	for _, ref := range []string{tmpl.Image, r.NetConfigImage} {
		if d, ok := lab.Status.ImageDigests[ref]; ok {
			out[ref] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
