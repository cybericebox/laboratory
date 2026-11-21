package laboratory

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func upstreamAuths(t *testing.T, s *corev1.Secret) map[string]struct{ Username, Password string } {
	t.Helper()
	var cfg struct {
		Auths map[string]struct{ Username, Password string } `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Auths
}

// The tenant's credentials are copied, as they are, into tenant-<name> of the namespace only the gate reads; no username convention.
func TestTenantCredentialsGoToTheUpstreamNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := secretScheme(t)
	src := dockerSecret("acme-registry", names.TenantsNamespace, map[string][2]string{"ghcr.io": {"bot", "tp"}, "docker.io": {"", "token-only"}})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantWithPullSecret("acme", "acme-registry"), src).
		WithStatusSubresource(&laboratoryv1alpha1.Tenant{}).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme, CacheEnabled: true}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > credentialRecheck {
		t.Errorf("the operator cannot watch Secrets: it looks again by itself: %v", res.RequeueAfter)
	}
	var got corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: names.UpstreamNamespace, Name: names.UpstreamTenantSecret("acme")}, &got); err != nil {
		t.Fatal(err)
	}
	if a := upstreamAuths(t, &got); len(a) != 2 || a["ghcr.io"].Username != "bot" || a["ghcr.io"].Password != "tp" {
		t.Fatalf("the document is copied as it is: %+v", a)
	}
	// The source changes: the copy follows it.
	var cur corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: names.TenantsNamespace, Name: "acme-registry"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Data = dockerSecret("x", "y", map[string][2]string{"ghcr.io": {"bot", "new"}}).Data
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: names.UpstreamNamespace, Name: names.UpstreamTenantSecret("acme")}, &got); err != nil || upstreamAuths(t, &got)["ghcr.io"].Password != "new" {
		t.Fatalf("%v", err)
	}
	// The tenant drops its Secret: the copy goes. So does the copy of a cache that is off.
	var ten laboratoryv1alpha1.Tenant
	if err := c.Get(ctx, req.NamespacedName, &ten); err != nil {
		t.Fatal(err)
	}
	ten.Spec.Images.PullSecret = ""
	if err := c.Update(ctx, &ten); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: names.UpstreamNamespace, Name: names.UpstreamTenantSecret("acme")}, &got); err == nil {
		t.Error("a tenant without credentials has no copy")
	}
}

func TestTenantCredentialsSourceMustBeARegistrySecret(t *testing.T) {
	ctx := context.Background()
	scheme := secretScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantWithPullSecret("acme", "missing"), tenantWithPullSecret("beta", "opaque"),
		tenantRegSecret("opaque", corev1.SecretTypeOpaque, "x")).WithStatusSubresource(&laboratoryv1alpha1.Tenant{}).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme, CacheEnabled: true}
	for _, name := range []string{"acme", "beta"} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err == nil {
			t.Errorf("%s: a missing or non-registry source must be an error, so the tenant is looked at again", name)
		}
	}
	// With the cache off nothing is written.
	r.CacheEnabled = false
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformCredentialsGoToTheUpstreamNamespace(t *testing.T) {
	ctx := context.Background()
	a := dockerSecret("ghcr-pull", names.SystemNamespace, map[string][2]string{"ghcr.io": {"robot", "rp"}, "quay.io": {"old", "old"}})
	b := dockerSecret("quay-pull", names.SystemNamespace, map[string][2]string{"quay.io": {"new", "np"}})
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(a, b).Build()
	if err := syncUpstreamPlatform(ctx, c, true, []string{"ghcr-pull", "quay-pull"}); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: names.UpstreamNamespace, Name: names.UpstreamPlatformSecret}, &got); err != nil {
		t.Fatal(err)
	}
	auths := upstreamAuths(t, &got)
	if len(auths) != 2 || auths["ghcr.io"].Username != "robot" || auths["quay.io"].Username != "new" {
		t.Fatalf("a later secret wins for one registry: %+v", auths)
	}
	if err := syncUpstreamPlatform(ctx, c, true, []string{"missing"}); err == nil {
		t.Error("a missing source is an error, never skipped")
	}
	if err := syncUpstreamPlatform(ctx, c, false, []string{"ghcr-pull"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: names.UpstreamNamespace, Name: names.UpstreamPlatformSecret}, &got); err == nil {
		t.Error("with the cache off the platform's copy is removed")
	}
}

func TestSecretOfAGoneLabIsDeletedAndLegacyPullSecretsAreRemoved(t *testing.T) {
	ctx := context.Background()
	lab := names.UpstreamLabSecret("team", "l1")
	legacy := dockerSecret("tenant-registry", "team", map[string][2]string{"ghcr.io": {"a", "b"}})
	legacy.Labels = map[string]string{managedByLabel: managedByValue}
	other := dockerSecret("keep-me", "team", map[string][2]string{"ghcr.io": {"a", "b"}})
	opaque := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "team", Labels: map[string]string{managedByLabel: managedByValue}}}
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(dockerSecret(lab, names.UpstreamNamespace, nil), legacy, other, opaque).Build()
	if err := deleteUpstreamLab(ctx, c, "team", "l1"); err != nil {
		t.Fatal(err)
	}
	if err := deleteUpstreamLab(ctx, c, "team", "l1"); err != nil {
		t.Fatalf("deleting twice is fine: %v", err)
	}
	if err := removeLegacyPullSecrets(ctx, c, "team"); err != nil {
		t.Fatal(err)
	}
	var list corev1.SecretList
	if err := c.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, s := range list.Items {
		left = append(left, s.Namespace+"/"+s.Name)
	}
	if len(left) != 2 {
		t.Fatalf("only the unlabelled registry Secret and the opaque one stay: %v", left)
	}
}
