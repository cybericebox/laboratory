package laboratory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=tenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// DefaultEnrollmentTTL is how long an unused enrollment token works.
const DefaultEnrollmentTTL = 24 * time.Hour

// TenantReconciler gives every Tenant a one-time enrollment token. The token is random; only
// its hash and expiry go to Tenant.status.enrollment, and the token itself is shown once, in
// the Secret tenant-<name>-enrollment of the tenants namespace (the admin reads it with
// kubectl). The agent burns the token when a client enrolls (status.enrollment.usedAt), and the
// Secret goes away. An annotation asks for a new token.
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// TTL is the lifetime of a token; zero means DefaultEnrollmentTTL.
	TTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CacheEnabled: the image cache is on, so the credentials of images.pullSecret must be usable with it (see checkImageCredentials).
	CacheEnabled bool
	// Recorder says it in an event; nil: no events.
	Recorder record.EventRecorder
}

// credentialRecheck is how often a tenant's pull Secret is read again for checkImageCredentials: the operator cannot watch Secrets.
const credentialRecheck = 5 * time.Minute

func (r *TenantReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// NewEnrollmentToken returns a fresh random token and its hash.
func NewEnrollmentToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashEnrollmentToken(token), nil
}

// HashEnrollmentToken is the hex SHA-256 of a token, what the status keeps.
func HashEnrollmentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcileEnrollment(ctx, req)
	if err != nil {
		return res, err
	}
	var t laboratoryv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil || t.DeletionTimestamp != nil {
		return res, client.IgnoreNotFound(err)
	}
	if err := r.checkImageCredentials(ctx, &t); err != nil {
		return res, err
	}
	if r.CacheEnabled && t.Spec.Images.PullSecret != "" && (res.RequeueAfter == 0 || res.RequeueAfter > credentialRecheck) {
		res.RequeueAfter = credentialRecheck
	}
	return res, nil
}

// checkImageCredentials makes the condition ImageCredentials of a tenant say whether the credentials of its images.pullSecret can be
// carried by the image cache. The cache writes the tenant into the username ("<tenant>~<user>"), so an entry without a username can
// never pull through it: the condition is False (reason CredentialNotSupported, message CREDENTIAL_NOT_SUPPORTED naming the registries)
// and a Warning event says it once, instead of the pods silently pulling anonymously and failing later. Labs of the tenant are refused
// with the same reason (LabReconciler.tenantCredentialRefusal, and the agent at creation). With the cache off, or no Secret, the
// condition is removed; a Secret that cannot be read changes nothing (the group reports it).
func (r *TenantReconciler) checkImageCredentials(ctx context.Context, t *laboratoryv1alpha1.Tenant) error {
	var bad []string
	if r.CacheEnabled && t.Spec.Images.PullSecret != "" {
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: names.TenantsNamespace, Name: t.Spec.Images.PullSecret}, &s); err != nil {
			return client.IgnoreNotFound(err)
		}
		bad, _ = imagecache.WithoutUsername(s.Data[corev1.DockerConfigJsonKey])
	}
	orig := t.DeepCopy()
	if len(bad) == 0 {
		if !meta.RemoveStatusCondition(&t.Status.Conditions, laboratoryv1alpha1.TenantConditionImageCredentials) {
			return nil
		}
	} else {
		msg := CredentialRefusalMessage(bad)
		if c := meta.FindStatusCondition(t.Status.Conditions, laboratoryv1alpha1.TenantConditionImageCredentials); c != nil && c.Status == metav1.ConditionFalse && c.Message == msg {
			return nil
		}
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type: laboratoryv1alpha1.TenantConditionImageCredentials, Status: metav1.ConditionFalse,
			Reason: labstatus.ReasonCredentialNotSupported, Message: msg, ObservedGeneration: t.Generation,
		})
		if r.Recorder != nil {
			r.Recorder.Event(t, corev1.EventTypeWarning, labstatus.ReasonCredentialNotSupported, msg)
		}
	}
	return r.Status().Patch(ctx, t, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// CredentialRefusalMessage is the text of the refusal of credentials the image cache cannot carry, with the stable reason first.
func CredentialRefusalMessage(registries []string) string {
	return "CREDENTIAL_NOT_SUPPORTED: registry " + strings.Join(registries, ", ") + ": credentials need a username for the image cache"
}

func (r *TenantReconciler) reconcileEnrollment(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t laboratoryv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &t); err != nil {
		if apierrors.IsNotFound(err) {
			// A deleted tenant leaves nothing behind: a tenant created later under the same
			// name must not inherit its access keys (they would verify its signatures) or token.
			return ctrl.Result{}, r.deleteTenantSecrets(ctx, req.Name)
		}
		return ctrl.Result{}, err
	}
	if t.DeletionTimestamp != nil {
		return ctrl.Result{}, r.deleteTokenSecret(ctx, t.Name)
	}
	_, regenerate := t.Annotations[names.AnnotationRegenerateEnrollment]
	en := t.Status.Enrollment
	switch {
	case regenerate || en == nil || en.TokenHash == "":
		return ctrl.Result{}, r.issue(ctx, &t)
	case en.UsedAt != nil:
		// shown once: the use burns it, and the token is no longer findable
		if err := r.syncTokenLabel(ctx, &t, false); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.deleteTokenSecret(ctx, t.Name)
	case en.ExpiresAt != nil && !r.now().Before(en.ExpiresAt.Time):
		return ctrl.Result{}, r.deleteTokenSecret(ctx, t.Name) // expired: the admin regenerates
	case en.ExpiresAt != nil:
		// A token issued before the label existed gets it now.
		if err := r.syncTokenLabel(ctx, &t, false); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: en.ExpiresAt.Sub(r.now()) + time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// issue generates a token, shows it in the Secret and records its hash and expiry.
func (r *TenantReconciler) issue(ctx context.Context, t *laboratoryv1alpha1.Tenant) error {
	token, hash, err := NewEnrollmentToken()
	if err != nil {
		return err
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = DefaultEnrollmentTTL
	}
	now := r.now()
	issued, expires := metav1.NewTime(now), metav1.NewTime(now.Add(ttl))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: names.EnrollmentSecret(t.Name), Namespace: names.TenantsNamespace,
			Labels:      map[string]string{names.LabelPrefix + "tenant-name": t.Name},
			Annotations: map[string]string{names.LabelPrefix + "expires-at": expires.UTC().Format(time.RFC3339)},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{names.EnrollmentTokenKey: token},
	}
	if err := r.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := r.deleteTokenSecret(ctx, t.Name); err != nil {
			return err
		}
		if err := r.Create(ctx, secret); err != nil {
			return err
		}
	}
	orig := t.DeepCopy()
	t.Status.Enrollment = &laboratoryv1alpha1.TenantEnrollment{TokenHash: hash, IssuedAt: &issued, ExpiresAt: &expires}
	if err := r.Status().Patch(ctx, t, client.MergeFrom(orig)); err != nil {
		return err
	}
	// The label that lets the agent find the Tenant of a token; set after the hash is recorded, so a token is never findable
	// before it is valid.
	_, regenerate := t.Annotations[names.AnnotationRegenerateEnrollment]
	return r.syncTokenLabel(ctx, t, regenerate)
}

// syncTokenLabel makes the Tenant's token label match its unused token (none: no label), and drops the regenerate request when
// asked to.
func (r *TenantReconciler) syncTokenLabel(ctx context.Context, t *laboratoryv1alpha1.Tenant, dropRegenerate bool) error {
	orig := t.DeepCopy()
	changed := false
	want := ""
	if en := t.Status.Enrollment; en != nil && en.TokenHash != "" && en.UsedAt == nil {
		want = names.EnrollmentTokenLabel(en.TokenHash)
	}
	if got, has := t.Labels[names.LabelEnrollmentToken]; want == "" && has {
		delete(t.Labels, names.LabelEnrollmentToken)
		changed = true
	} else if want != "" && got != want {
		if t.Labels == nil {
			t.Labels = map[string]string{}
		}
		t.Labels[names.LabelEnrollmentToken] = want
		changed = true
	}
	if _, ok := t.Annotations[names.AnnotationRegenerateEnrollment]; ok && dropRegenerate {
		delete(t.Annotations, names.AnnotationRegenerateEnrollment)
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, t, client.MergeFrom(orig))
}

// deleteTenantSecrets removes the enrollment Secret and the access-keys Secret of a tenant that is gone.
func (r *TenantReconciler) deleteTenantSecrets(ctx context.Context, tenant string) error {
	if err := r.deleteTokenSecret(ctx, tenant); err != nil {
		return err
	}
	err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.AccessKeysSecret(tenant), Namespace: names.AccessKeysNamespace}})
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	return nil
}

func (r *TenantReconciler) deleteTokenSecret(ctx context.Context, tenant string) error {
	err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.EnrollmentSecret(tenant), Namespace: names.TenantsNamespace}})
	return client.IgnoreNotFound(err)
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.Tenant{}).
		Named("tenant-enrollment").
		Complete(reconcileutil.Quiet(r))
}
