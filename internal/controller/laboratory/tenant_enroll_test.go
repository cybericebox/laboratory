package laboratory

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func tenantEnrollRig(t *testing.T) (*TenantReconciler, client.Client, *time.Time) {
	t.Helper()
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}).
		WithStatusSubresource(&laboratoryv1alpha1.Tenant{}).Build()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return &TenantReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}, c, &now
}

func reconcileTenant(t *testing.T, r *TenantReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getTenant(t *testing.T, c client.Client) *laboratoryv1alpha1.Tenant {
	t.Helper()
	var ten laboratoryv1alpha1.Tenant
	if err := c.Get(context.Background(), types.NamespacedName{Name: "acme"}, &ten); err != nil {
		t.Fatal(err)
	}
	return &ten
}

func tokenSecret(c client.Client) (string, error) {
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: names.TenantsNamespace, Name: names.EnrollmentSecret("acme")}, &s); err != nil {
		return "", err
	}
	return string(s.Data[names.EnrollmentTokenKey]) + s.StringData[names.EnrollmentTokenKey], nil
}

func TestTenantGetsAnEnrollmentTokenShownOnceAndStoredHashed(t *testing.T) {
	r, c, now := tenantEnrollRig(t)
	res := reconcileTenant(t, r)
	token, err := tokenSecret(c)
	if err != nil || len(token) < 40 {
		t.Fatalf("the token is shown in the Secret: %q %v", token, err)
	}
	en := getTenant(t, c).Status.Enrollment
	if en == nil || en.TokenHash != HashEnrollmentToken(token) || en.TokenHash == token {
		t.Fatalf("only the hash is stored: %+v", en)
	}
	if !en.ExpiresAt.Time.Equal(now.Add(DefaultEnrollmentTTL)) || en.UsedAt != nil {
		t.Fatalf("expiry %+v", en)
	}
	if res.RequeueAfter != 0 {
		// The first pass writes; the next one plans the expiry.
		t.Fatalf("%v", res)
	}
	if res = reconcileTenant(t, r); res.RequeueAfter < DefaultEnrollmentTTL {
		t.Fatalf("the reconciler wakes up at the expiry: %v", res)
	}
	// Reconciling again must not replace a live token.
	if again, _ := tokenSecret(c); again != token {
		t.Fatal("a live token stays")
	}
}

func TestEnrollmentTokenIsBurntByUseAndRegeneratedOnRequest(t *testing.T) {
	r, c, now := tenantEnrollRig(t)
	reconcileTenant(t, r)
	first, _ := tokenSecret(c)

	// Use: the agent sets usedAt, the operator takes the token away.
	ten := getTenant(t, c)
	used := metav1.NewTime(*now)
	ten.Status.Enrollment.UsedAt = &used
	if err := c.Status().Update(context.Background(), ten); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	if _, err := tokenSecret(c); !apierrors.IsNotFound(err) {
		t.Fatalf("a used token is no longer shown: %v", err)
	}
	reconcileTenant(t, r)
	if _, err := tokenSecret(c); !apierrors.IsNotFound(err) {
		t.Fatal("and it is not made again by itself")
	}

	// The annotation asks for a new token.
	ten = getTenant(t, c)
	ten.Annotations = map[string]string{names.AnnotationRegenerateEnrollment: "true"}
	if err := c.Update(context.Background(), ten); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	second, err := tokenSecret(c)
	if err != nil || second == first {
		t.Fatalf("a new token: %q %v", second, err)
	}
	ten = getTenant(t, c)
	if _, still := ten.Annotations[names.AnnotationRegenerateEnrollment]; still || ten.Status.Enrollment.UsedAt != nil || ten.Status.Enrollment.TokenHash != HashEnrollmentToken(second) {
		t.Fatalf("the request is consumed and the token is fresh: %+v %v", ten.Status.Enrollment, ten.Annotations)
	}

	// Regenerating while an unused token exists replaces it.
	ten.Annotations = map[string]string{names.AnnotationRegenerateEnrollment: "true"}
	if err := c.Update(context.Background(), ten); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r)
	if third, _ := tokenSecret(c); third == second || third == "" {
		t.Fatal("replaced")
	}
}

func TestExpiredTokenGoesAwayUntilRegenerated(t *testing.T) {
	r, c, now := tenantEnrollRig(t)
	reconcileTenant(t, r)
	*now = now.Add(DefaultEnrollmentTTL + time.Minute)
	reconcileTenant(t, r)
	if _, err := tokenSecret(c); !apierrors.IsNotFound(err) {
		t.Fatalf("an expired token is removed: %v", err)
	}
	r.TTL = time.Hour
	ten := getTenant(t, c)
	ten.Annotations = map[string]string{names.AnnotationRegenerateEnrollment: "true"}
	_ = c.Update(context.Background(), ten)
	reconcileTenant(t, r)
	if got := getTenant(t, c).Status.Enrollment.ExpiresAt.Time; !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("the configured lifetime applies: %v", got)
	}
}
