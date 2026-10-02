package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

func secretScheme(t *testing.T) *runtime.Scheme {
	s := pruneScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func tenantWithPullSecret(name, secret string) *laboratoryv1alpha1.Tenant {
	return &laboratoryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       laboratoryv1alpha1.TenantSpec{Images: laboratoryv1alpha1.TenantImages{PullSecret: secret}},
	}
}

func tenantRegSecret(name string, typ corev1.SecretType, data string) *corev1.Secret {
	s := regSecret(name, typ, data)
	s.Namespace = names.TenantsNamespace
	return s
}

func TestSyncTenantPullSecretCopiesTheTenantsCredentials(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		tenantWithPullSecret("acme", "acme-registry"),
		tenantRegSecret("acme-registry", corev1.SecretTypeDockerConfigJson, `{"auths":{"r":{}}}`),
		// A platform pull secret of the same release namespace is never the tenant's.
		regSecret("platform", corev1.SecretTypeDockerConfigJson, `{"auths":{"platform":{}}}`),
	).Build()

	if err := syncTenantPullSecret(ctx, c, "acme", "team-a"); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: names.TenantPullSecret, Namespace: "team-a"}, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data[corev1.DockerConfigJsonKey]) != `{"auths":{"r":{}}}` {
		t.Fatalf("copy: %s", got.Data[corev1.DockerConfigJsonKey])
	}

	// The credentials follow the source; a tenant without any gets none.
	if err := syncTenantPullSecret(ctx, c, "stranger", "team-b"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: names.TenantPullSecret, Namespace: "team-b"}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("no credentials, no copy: %v", err)
	}

	// Removing the reference removes the copy.
	ten := tenantWithPullSecret("acme", "")
	var cur laboratoryv1alpha1.Tenant
	_ = c.Get(ctx, types.NamespacedName{Name: "acme"}, &cur)
	cur.Spec = ten.Spec
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	if err := syncTenantPullSecret(ctx, c, "acme", "team-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: names.TenantPullSecret, Namespace: "team-a"}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("the copy must go with the reference: %v", err)
	}
}

func TestSyncTenantPullSecretRefusesAMissingOrWrongSource(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		tenantWithPullSecret("gone", "absent"),
		tenantWithPullSecret("opaque", "plain"),
		tenantRegSecret("plain", corev1.SecretTypeOpaque, "x"),
	).Build()
	if err := syncTenantPullSecret(context.Background(), c, "gone", "ns"); err == nil {
		t.Error("a missing source is an error")
	}
	if err := syncTenantPullSecret(context.Background(), c, "opaque", "ns"); err == nil {
		t.Error("a non-registry source is an error")
	}
}

// Device pods get the tenant's credentials and never the platform's.
func TestDevicePodsGetTheTenantsPullSecretOnly(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		tenantWithPullSecret("acme", "acme-registry"), tenantWithPullSecret("plain", ""),
	).Build()
	r := &DeviceReconciler{Client: c}
	dev := func(tenant string) *laboratoryv1alpha1.Device {
		return &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Labels: map[string]string{names.LabelTenant: tenant}}}
	}
	got := r.devicePullSecrets(context.Background(), dev("acme"))
	if len(got) != 1 || got[0].Name != names.TenantPullSecret {
		t.Fatalf("acme: %v", got)
	}
	for _, tn := range []string{"plain", "unknown"} {
		if got := r.devicePullSecrets(context.Background(), dev(tn)); got != nil {
			t.Errorf("%s: %v", tn, got)
		}
	}
	_, _, _, spec := r.workloadTemplate(dev("acme"), false)
	if len(spec.ImagePullSecrets) != 0 {
		t.Fatalf("the template itself carries no pull secret: %v", spec.ImagePullSecrets)
	}
}

// A tenant with registry credentials of its own pulls directly: its lab does not use the shared cache.
func TestTenantWithOwnCredentialsBypassesTheImageCache(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		tenant string
		cache  bool
	}{{"acme", false}, {"plain", true}, {"", true}} {
		lab := newLab("l-" + tc.tenant)
		if tc.tenant != "" {
			lab.Labels = map[string]string{names.LabelTenant: tc.tenant}
		}
		r := cachedLabReconciler(t, true, lab, tenantWithPullSecret("acme", "acme-registry"), tenantWithPullSecret("plain", ""))
		r.Mirror = imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
		if _, err := r.ensureModes(ctx, lab); err != nil {
			t.Fatal(err)
		}
		if *lab.Status.ImageCache != tc.cache {
			t.Errorf("tenant %q: cache=%v, want %v", tc.tenant, *lab.Status.ImageCache, tc.cache)
		}
	}
}
