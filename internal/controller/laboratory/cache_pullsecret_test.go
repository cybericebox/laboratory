package laboratory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
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
