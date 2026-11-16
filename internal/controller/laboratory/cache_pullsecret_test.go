package laboratory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

func dockerSecret(name, ns string, auths map[string][2]string) *corev1.Secret {
	m := map[string]map[string]string{}
	for host, up := range auths {
		m[host] = map[string]string{"username": up[0], "password": up[1], "auth": base64.StdEncoding.EncodeToString([]byte(up[0] + ":" + up[1]))}
	}
	raw, _ := json.Marshal(map[string]any{"auths": m})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
}

func cachedDevice(lab, ns, tenant string) *laboratoryv1alpha1.Device {
	d := labDevice(lab, ns, tenant)
	d.Spec.ImageMirror = "localhost:5035"
	return d
}

// A device on the cache carries a Secret that the operator made: the credentials of the tenant and of the lab under the cache path of
// their registry, the tenant in the username, the lab's own over the tenant's. The raw Secrets are not referenced at all.
func TestDeviceOnTheCacheGetsTheRekeyedPullSecret(t *testing.T) {
	ctx := context.Background()
	lab := labWithPullSecret("l1", "team", "l1-registry", map[string]string{names.LabelTenant: "acme"})
	lab.UID = "uid-1"
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		tenantWithPullSecret("acme", "acme-registry"), lab,
		dockerSecret(names.TenantPullSecret, "team", map[string][2]string{"ghcr.io": {"tenant-bot", "tp"}, "docker.io": {"hub", "hp"}}),
		dockerSecret("l1-registry", "team", map[string][2]string{"ghcr.io": {"lab-bot", "lp"}, "registry.example.com": {"x", "y"}}),
	).Build()
	r := &DeviceReconciler{Client: c, MirrorRegistries: imagecache.DefaultRegistries}
	dev := cachedDevice("l1", "team", "acme")

	spec := pullSpec()
	if err := r.applyPullSecrets(ctx, dev, spec); err != nil {
		t.Fatal(err)
	}
	want := names.LabCacheRegistrySecret("l1")
	if got := refNames(spec.ImagePullSecrets); len(got) != 1 || got[0] != want {
		t.Fatalf("only the re-keyed Secret is referenced, got %v", got)
	}
	if spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("a pod with credentials asks the cache on every start: %s", spec.Containers[0].ImagePullPolicy)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team", Name: want}, &s); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password string } `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auths) != 2 ||
		cfg.Auths["localhost:5035/c/ghcr.io"].Username != "acme~lab-bot" || cfg.Auths["localhost:5035/c/ghcr.io"].Password != "lp" ||
		cfg.Auths["localhost:5035/c/docker.io"].Username != "acme~hub" {
		t.Fatalf("%+v", cfg.Auths)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Lab" || s.OwnerReferences[0].UID != "uid-1" {
		t.Errorf("the Secret goes with the lab: %+v", s.OwnerReferences)
	}
	// asked again: the same Secret, not another
	if err := r.applyPullSecrets(ctx, dev, pullSpec()); err != nil {
		t.Fatal(err)
	}
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace("team")); err != nil || len(list.Items) != 3 {
		t.Fatalf("%d secrets %v", len(list.Items), err)
	}
}

// The tenant in the username is the tenant label of the device, never anything else; a device without credentials gets no Secret, and
// one whose credentials are all for registries the cache does not serve loses a Secret that was there.
func TestDeviceOnTheCacheWithoutUsableCredentialsGetsNoSecret(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		labWithPullSecret("l2", "team", "", nil),
		labWithPullSecret("l3", "team", "l3-registry", nil),
		dockerSecret("l3-registry", "team", map[string][2]string{"registry.example.com": {"x", "y"}}),
		dockerSecret(names.LabCacheRegistrySecret("l3"), "team", map[string][2]string{"localhost:5035/c/ghcr.io": {"stale", "stale"}}),
	).Build()
	r := &DeviceReconciler{Client: c, MirrorRegistries: imagecache.DefaultRegistries}
	for _, lab := range []string{"l2", "l3"} {
		spec := pullSpec()
		if err := r.applyPullSecrets(ctx, cachedDevice(lab, "team", ""), spec); err != nil {
			t.Fatal(err)
		}
		if len(spec.ImagePullSecrets) != 0 || spec.Containers[0].ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("%s: %v", lab, spec.ImagePullSecrets)
		}
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team", Name: names.LabCacheRegistrySecret("l3")}, &s); err == nil {
		t.Error("a Secret with nothing usable in it is removed")
	}
}

// A device that is not on the cache keeps the raw Secrets, as before.
func TestDeviceOffTheCacheKeepsTheRawPullSecrets(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		labWithPullSecret("l1", "team", "l1-registry", nil),
		dockerSecret("l1-registry", "team", map[string][2]string{"ghcr.io": {"a", "b"}}),
	).Build()
	r := &DeviceReconciler{Client: c, MirrorRegistries: imagecache.DefaultRegistries}
	spec := pullSpec()
	if err := r.applyPullSecrets(ctx, labDevice("l1", "team", ""), spec); err != nil {
		t.Fatal(err)
	}
	if got := refNames(spec.ImagePullSecrets); len(got) != 1 || got[0] != "l1-registry" {
		t.Fatalf("%v", got)
	}
}

// The platform's own pods of a group (VPN, gateway) pull with the operator's secrets re-keyed for the cache, tenant "platform".
func TestPlatformPodsGetTheRekeyedSecretWhenTheCacheIsOn(t *testing.T) {
	ctx := context.Background()
	src := dockerSecret("ghcr-pull", names.SystemNamespace, map[string][2]string{"ghcr.io": {"robot", "rp"}, "registry.example.com": {"x", "y"}})
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(src).Build()
	mirror := imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
	if err := syncPlatformCacheSecret(ctx, c, mirror, []string{"ghcr-pull"}, "group-ns"); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "group-ns", Name: names.PlatformCacheSecret}, &s); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Username string } `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil || len(cfg.Auths) != 1 || cfg.Auths["localhost:5035/c/ghcr.io"].Username != "platform~robot" {
		t.Fatalf("%+v %v", cfg.Auths, err)
	}
	g := &LabGroupReconciler{Mirror: mirror, ImagePullSecrets: []string{"ghcr-pull"}}
	if got := refNames(g.groupPullSecretRefs()); len(got) != 1 || got[0] != names.PlatformCacheSecret {
		t.Fatalf("%v", got)
	}
	// cache off: the raw ones, and the derived Secret goes away
	off := &LabGroupReconciler{ImagePullSecrets: []string{"ghcr-pull"}}
	if got := refNames(off.groupPullSecretRefs()); len(got) != 1 || got[0] != "ghcr-pull" {
		t.Fatalf("%v", got)
	}
	if err := syncPlatformCacheSecret(ctx, c, imagecache.Rewriter{}, []string{"ghcr-pull"}, "group-ns"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "group-ns", Name: names.PlatformCacheSecret}, &s); err == nil {
		t.Error("with the cache off the re-keyed Secret is removed")
	}
}

// With the cache on an image that cannot go through it is refused: the lab at admission, the device at its start (the list of served
// registries may change under it), and never pulled directly.
func TestTheCacheOnRefusesWhatItCannotServe(t *testing.T) {
	mirror := imagecache.Rewriter{Prefix: "localhost:5035", Registries: []string{"docker.io", "ghcr.io"}}
	lab := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "a", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "ghcr.io/acme/web:1"},
		{Name: "b", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "registry.example.com/acme/db:1"},
	}}}
	r := &LabReconciler{Mirror: mirror, AllowedRegistries: []string{"*"}}
	if err := r.validateRegistries(lab); err == nil || !strings.Contains(err.Error(), "REGISTRY_NOT_ALLOWED") || !strings.Contains(err.Error(), `device "b"`) {
		t.Fatalf("the second device cannot go through the cache: %v", err)
	}
	if err := (&LabReconciler{AllowedRegistries: []string{"*"}}).validateRegistries(lab); err != nil {
		t.Fatalf("with the cache off the registry list alone decides: %v", err)
	}
	dev := cachedDevice("l1", "team", "")
	dev.Spec.Image = "registry.example.com/acme/db:1"
	d := &DeviceReconciler{MirrorRegistries: mirror.Registries}
	if err := d.refuseUncached(dev); err == nil {
		t.Fatal("a device on the cache whose image the cache does not serve must not start")
	}
	dev.Spec.Image = "ghcr.io/acme/web:1"
	if err := d.refuseUncached(dev); err != nil {
		t.Fatal(err)
	}
	dev.Spec.ImageMirror = ""
	dev.Spec.Image = "registry.example.com/acme/db:1"
	if err := d.refuseUncached(dev); err != nil {
		t.Fatalf("a device that was made without the cache keeps its mode: %v", err)
	}
}

const digB = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// credLab is a lab of the tenant with the raw pull Secret the agent wrote for it ("ghcr.io" or "nginx"'s registry is docker.io).
func credLab(name, tenant, user string) (*laboratoryv1alpha1.Lab, *corev1.Secret) {
	lab := pinLab()
	lab.Name, lab.Namespace = name, "team"
	lab.Spec.ImagePullSecret = name + "-registry"
	if tenant != "" {
		lab.Labels = map[string]string{names.LabelTenant: tenant}
	}
	return lab, dockerSecret(lab.Spec.ImagePullSecret, "team", map[string][2]string{"docker.io": {user, "pw-" + user}})
}

// A lab with credentials of its own is pinned too, resolved with those credentials only (never the operator's pull secrets, and never a
// digest that another credential set got), and the netconfig image, which is the platform's, with the operator's.
func TestLabWithCredentialsIsPinnedWithItsOwnCredentials(t *testing.T) {
	ctx := context.Background()
	opRes := &fakeResolver{digests: map[string]string{"nginx:1.25": "sha256:operator-would-say-this", "ghcr.io/cybericebox/laboratory-node:v1": digN}}
	credRes := map[string]*fakeResolver{
		"alice": {digests: map[string]string{"nginx:1.25": digA}},
		"bob":   {digests: map[string]string{"nginx:1.25": digB}},
	}
	var keychains []string
	labA, secA := credLab("la", "", "alice")
	labA2, secA2 := credLab("la2", "", "alice") // the same credentials as la
	labB, secB := credLab("lb", "", "bob")
	r := cachedLabReconciler(t, true, labA, secA, labA2, secA2, labB, secB)
	r.Pinner, r.NetConfigImage = pinnerOf(opRes), "ghcr.io/cybericebox/laboratory-node:v1"
	r.CredentialResolver = func(kc authn.Keychain) imagecache.Resolver {
		a, err := kc.Resolve(mustRepo(t, "docker.io/library/nginx"))
		if err != nil {
			t.Fatal(err)
		}
		ac, _ := a.Authorization()
		keychains = append(keychains, ac.Username)
		return credRes[ac.Username]
	}
	for _, lab := range []*laboratoryv1alpha1.Lab{labA, labA2, labB} {
		if _, err := settleModes(ctx, r, lab); err != nil {
			t.Fatalf("%s: %v", lab.Name, err)
		}
	}
	if got := labA.Status.ImageDigests["nginx:1.25"]; got != digA {
		t.Errorf("alice's lab is pinned with alice's credentials: %q", got)
	}
	if got := labA2.Status.ImageDigests["nginx:1.25"]; got != digA {
		t.Errorf("the same credentials give the same answer: %q", got)
	}
	if got := labB.Status.ImageDigests["nginx:1.25"]; got != digB {
		t.Errorf("another credential set never gets alice's digest: %q", got)
	}
	if len(keychains) != 2 {
		t.Errorf("one resolver per credential fingerprint, got %v", keychains)
	}
	if len(credRes["alice"].called()) != 1 {
		t.Errorf("one resolution for the credentials of two labs: %v", credRes["alice"].called())
	}
	for _, c := range opRes.called() {
		if c == "nginx:1.25" {
			t.Error("the operator's own pull secrets must not resolve a lab's image")
		}
	}
	if labA.Status.ImageDigests["ghcr.io/cybericebox/laboratory-node:v1"] != digN {
		t.Errorf("the netconfig image is the platform's: %v", labA.Status.ImageDigests)
	}
}

// The agent writes the lab's Secret right after the Lab: until it is there the lab waits, and decides nothing.
func TestLabWaitsForItsPullSecretBeforePinning(t *testing.T) {
	ctx := context.Background()
	lab, sec := credLab("l1", "", "alice")
	r := cachedLabReconciler(t, true, lab)
	r.Pinner = pinnerOf(&fakeResolver{})
	r.CredentialResolver = func(authn.Keychain) imagecache.Resolver {
		return &fakeResolver{digests: map[string]string{"nginx:1.25": digA}}
	}
	if _, err := r.ensureModes(ctx, lab); !reconcileutil.IsWaiting(err) || !errors.Is(err, errPullSecretPending) || lab.Status.ImageCache != nil {
		t.Fatalf("%v %v", err, lab.Status.ImageCache)
	}
	if err := r.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := settleModes(ctx, r, lab); err != nil || lab.Status.ImageDigests["nginx:1.25"] != digA {
		t.Fatalf("%v %v", err, lab.Status.ImageDigests)
	}
}

// A failed resolution with the lab's credentials falls back to the tag and says so, as for any lab.
func TestLabWithCredentialsFallsBackToTheTagWhenResolvingFails(t *testing.T) {
	ctx := context.Background()
	lab, sec := credLab("l1", "", "alice")
	r := cachedLabReconciler(t, true, lab, sec)
	r.Pinner = pinnerOf(&fakeResolver{})
	r.CredentialResolver = func(authn.Keychain) imagecache.Resolver {
		return &fakeResolver{fail: map[string]bool{"nginx:1.25": true}}
	}
	if _, err := settleModes(ctx, r, lab); err != nil {
		t.Fatal(err)
	}
	if !*lab.Status.ImageCache || lab.Status.ImageDigests["nginx:1.25"] != "" || !strings.Contains(lab.Status.ImageWarning, "nginx:1.25") {
		t.Fatalf("%v %v %q", *lab.Status.ImageCache, lab.Status.ImageDigests, lab.Status.ImageWarning)
	}
}

func mustRepo(t *testing.T, s string) name.Repository {
	t.Helper()
	repo, err := name.NewRepository(s)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func tenantWithCreds(name, secret string) *laboratoryv1alpha1.Tenant {
	return tenantWithPullSecret(name, secret)
}

func tenantSecretWith(name string, auths map[string][2]string) *corev1.Secret {
	s := dockerSecret(name, names.TenantsNamespace, auths)
	return s
}

// A tenant whose images.pullSecret has an entry without a username cannot use the cache: the Tenant says so in its condition and an event,
// naming the registry, and the lab of that tenant is refused with the same reason; fixing the Secret clears both.
func TestTenantCredentialsWithoutAUsernameAreRefusedWhileTheCacheIsOn(t *testing.T) {
	ctx := context.Background()
	bad := tenantSecretWith("acme-registry", map[string][2]string{"ghcr.io": {"", "token-only"}, "docker.io": {"u", "p"}})
	scheme := secretScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantWithCreds("acme", "acme-registry"), bad).
		WithStatusSubresource(&laboratoryv1alpha1.Tenant{}).Build()
	rec := record.NewFakeRecorder(10)
	r := &TenantReconciler{Client: c, Scheme: scheme, CacheEnabled: true, Recorder: rec, Now: time.Now}
	reconcile := func() ctrl.Result {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := reconcile(); res.RequeueAfter <= 0 || res.RequeueAfter > credentialRecheck {
		t.Errorf("the operator cannot watch Secrets: it looks again by itself: %v", res.RequeueAfter)
	}
	ten := getTenantNamed(t, c, "acme")
	cond := meta.FindStatusCondition(ten.Status.Conditions, laboratoryv1alpha1.TenantConditionImageCredentials)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "CredentialNotSupported" ||
		!strings.Contains(cond.Message, "CREDENTIAL_NOT_SUPPORTED: registry ghcr.io:") || strings.Contains(cond.Message, "docker.io") || strings.Contains(cond.Message, "token-only") {
		t.Fatalf("%+v", cond)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning") || !strings.Contains(ev, "registry ghcr.io") {
			t.Fatalf("event %q", ev)
		}
	default:
		t.Fatal("no event")
	}
	reconcile()
	select {
	case ev := <-rec.Events:
		t.Fatalf("the event is said once, not at every look: %q", ev)
	default:
	}

	// the lab of that tenant is refused with the same reason
	lab := newLab("l")
	lab.Labels = map[string]string{names.LabelTenant: "acme"}
	lr := &LabReconciler{Client: c, Mirror: imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}}
	if refusal, err := lr.tenantCredentialRefusal(ctx, lab); err != nil || refusal == nil || !strings.HasPrefix(refusal.Error(), "CREDENTIAL_NOT_SUPPORTED: registry ghcr.io") {
		t.Fatalf("%v %v", refusal, err)
	}
	if refusal, _ := (&LabReconciler{Client: c}).tenantCredentialRefusal(ctx, lab); refusal != nil {
		t.Fatalf("with the cache off nothing is refused: %v", refusal)
	}

	// fixed: the condition and the refusal are gone
	fixed := tenantSecretWith("acme-registry", map[string][2]string{"ghcr.io": {"bot", "token"}, "docker.io": {"u", "p"}})
	fixed.ResourceVersion = bad.ResourceVersion
	if err := c.Update(ctx, fixed); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if meta.FindStatusCondition(getTenantNamed(t, c, "acme").Status.Conditions, laboratoryv1alpha1.TenantConditionImageCredentials) != nil {
		t.Error("the condition goes with the problem")
	}
	if refusal, _ := lr.tenantCredentialRefusal(ctx, lab); refusal != nil {
		t.Errorf("%v", refusal)
	}
	// the cache off: no condition, no requeue for it
	r.CacheEnabled = false
	if res := reconcile(); res.RequeueAfter > 0 && res.RequeueAfter <= credentialRecheck {
		t.Errorf("no recheck without the cache: %v", res.RequeueAfter)
	}
}

func getTenantNamed(t *testing.T, c client.Client, name string) *laboratoryv1alpha1.Tenant {
	t.Helper()
	var ten laboratoryv1alpha1.Tenant
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &ten); err != nil {
		t.Fatal(err)
	}
	return &ten
}
