package laboratory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
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
	// Every lab goes through the cache when it is on, also one with registry credentials of its own or of its tenant: the gate proves each
	// pull with the pull's own credentials (re-keyed for the cache path by the Device controller), so a private image is never fetched with
	// anyone else's. Credentials only decide whether the tags are pinned (below).
	cache := r.Mirror.Enabled()
	if cache {
		var devices laboratoryv1alpha1.DeviceList
		if err := r.List(ctx, &devices, client.InNamespace(lab.Namespace), client.MatchingLabels{names.LabelLab: lab.Name}); err != nil {
			return false, err
		}
		if len(devices.Items) > 0 {
			cache = false
		}
	}
	// The tags are pinned at creation, so every device of the lab (and the labs of one wave) runs the same image if an upstream tag moves
	// mid-event. A lab with registry credentials of its own, or whose tenant has some, resolves them with those credentials (in memory,
	// one source per credential fingerprint), so a private image is pinned too; the pods then pull the digest through the gate with
	// the same credentials.
	if cache {
		source, pending, err := r.pinSource(ctx, lab)
		if err != nil {
			return false, err
		}
		if pending {
			return false, reconcileutil.Waiting(errPullSecretPending, pullSecretRetry)
		}
		pending, err = r.pinImages(ctx, lab, source)
		if err != nil {
			return false, err
		}
		if pending {
			// Nothing is decided or written until the digests are known; the Pinner wakes the lab when they are. The resolutions run
			// in the background, so the worker goes on with other labs.
			return false, reconcileutil.Waiting(errPinningPending, pinningRecheck)
		}
	}
	lab.Status.ImageCache = &cache
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

// errPinningPending is why a lab waits: some of its image tags are still being resolved in the background.
var errPinningPending = errors.New("pinning pending: the image digests are still being resolved")

// pinningRecheck is how soon a lab that waits for digests looks again if the Pinner did not wake it (a lost wake-up, a restart).
const pinningRecheck = 5 * time.Second

// sourceOf is the credential source a tag is resolved with: the lab's own, except the netconfig image, which is the platform's and is
// resolved with the operator's pull secrets whatever credentials the lab has.
func sourceOf(labSource, ref, netConfig string) string {
	if ref == netConfig {
		return imagecache.OperatorSource
	}
	return labSource
}

// pinSource is the credential source the tags of the lab are resolved with: the operator's own pull secrets for a lab with no credentials
// of its own or its tenant's, otherwise a source made of those credentials (the tenant's, then the lab's own over them), named by a
// fingerprint of the documents. pending says that the lab's own Secret, which the agent writes into the upstream namespace, is not there yet.
func (r *LabReconciler) pinSource(ctx context.Context, lab *laboratoryv1alpha1.Lab) (source string, pending bool, err error) {
	if r.Pinner == nil {
		return imagecache.OperatorSource, false, nil
	}
	var docs [][]byte
	secret, err := tenantPullSecret(ctx, r, names.TenantOf(lab.Labels))
	if err != nil {
		return "", false, err
	}
	if secret != "" {
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: secret, Namespace: names.TenantsNamespace}, &s); err != nil {
			return "", false, fmt.Errorf("tenant image pull secret %s/%s: %w", names.TenantsNamespace, secret, err)
		}
		docs = append(docs, s.Data[corev1.DockerConfigJsonKey])
	}
	if lab.Spec.ImagePullSecret != "" {
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: lab.Spec.ImagePullSecret, Namespace: names.UpstreamNamespace}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				return "", true, nil
			}
			return "", false, err
		}
		docs = append(docs, s.Data[corev1.DockerConfigJsonKey])
	}
	if len(docs) == 0 {
		return imagecache.OperatorSource, false, nil
	}
	if r.CredentialResolver == nil {
		return "", false, nil // nothing can resolve with them: the tags stay tags (imageWarning)
	}
	kc, err := imagecache.NewDockerConfigKeychain(docs...)
	if err != nil {
		return "", false, nil
	}
	h := sha256.New()
	for _, d := range docs {
		h.Write(d)
		h.Write([]byte{0})
	}
	source = imagecache.CredentialSource(hex.EncodeToString(h.Sum(nil))[:24])
	r.Pinner.EnsureSource(source, func() imagecache.Resolver { return r.CredentialResolver(kc) })
	return source, false, nil
}

// digestRequiredError is the refusal of a lab whose image tags could not be pinned to a digest while the cache requires digests.
type digestRequiredError struct{ images []string }

func (e *digestRequiredError) Error() string {
	return "IMAGE_DIGEST_REQUIRED: the image cache requires digests, and these tags could not be resolved: " + strings.Join(e.images, "; ")
}

// pinImages resolves the image tags of the lab's container devices, and the netconfig image, to digests once, at creation, and records
// them in the Lab status: the whole lab pulls exactly that content. An image that cannot be resolved is left to its tag and named in
// Status.ImageWarning. The resolving is done by the Pinner in the background: while any answer is missing pinImages touches nothing
// and reports pending (the lab asks again when the Pinner wakes it).
func (r *LabReconciler) pinImages(ctx context.Context, lab *laboratoryv1alpha1.Lab, source string) (pending bool, err error) {
	if r.Pinner == nil {
		return false, nil
	}
	var refs []string
	seen := map[string]bool{}
	add := func(ref string) {
		if ref != "" && !seen[ref] && r.Mirror.RewriteFor(names.TenantOf(lab.Labels), ref) != ref {
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
	waiter := lab.Namespace + "/" + lab.Name
	answers := make([]imagecache.Lookup, len(refs))
	for i, ref := range refs {
		answers[i] = r.Pinner.Lookup(ctx, sourceOf(source, ref, r.NetConfigImage), ref, waiter)
		if answers[i].Pending {
			pending = true
		}
	}
	if pending {
		return true, nil
	}
	digests := map[string]string{}
	var failed []string
	for i, ref := range refs {
		if answers[i].Err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", ref, answers[i].Err))
			continue
		}
		digests[ref] = answers[i].Digest
	}
	for _, ref := range refs {
		r.Pinner.Done(ctx, sourceOf(source, ref, r.NetConfigImage), waiter, ref)
	}
	if len(digests) > 0 {
		lab.Status.ImageDigests = digests
	}
	if len(failed) > 0 && r.RequireDigest {
		// The gate refuses a pull by tag (GATE_REQUIRE_DIGEST): a lab whose tag cannot be pinned cannot start, and says so now and not
		// as pods that fail to pull.
		return false, &digestRequiredError{images: failed}
	}
	if len(failed) > 0 {
		lab.Status.ImageWarning = "image tags not pinned to a digest, pulled by tag: " + strings.Join(failed, "; ")
		if r.Recorder != nil {
			r.Recorder.Event(lab, corev1.EventTypeWarning, "ImageNotPinned", lab.Status.ImageWarning)
		}
	}
	return false, nil
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
