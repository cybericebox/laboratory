package laboratory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// credLab is a lab of the tenant with the credentials Secret the agent wrote for it (in the upstream namespace) ("ghcr.io" or "nginx"'s registry is docker.io).
func credLab(name, tenant, user string) (*laboratoryv1alpha1.Lab, *corev1.Secret) {
	lab := pinLab()
	lab.Name, lab.Namespace = name, "team"
	lab.Spec.ImagePullSecret = names.UpstreamLabSecret("team", name)
	if tenant != "" {
		lab.Labels = map[string]string{names.LabelTenant: tenant}
	}
	return lab, dockerSecret(lab.Spec.ImagePullSecret, names.UpstreamNamespace, map[string][2]string{"docker.io": {user, "pw-" + user}})
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

// With digests required (the gate refuses a pull by tag) a tag that cannot be pinned fails the creation of the lab, and says which tags.
func TestRequireDigestFailsALabWhoseTagCannotBePinned(t *testing.T) {
	ctx := context.Background()
	lab, sec := credLab("l1", "", "alice")
	r := cachedLabReconciler(t, true, lab, sec)
	r.Pinner = pinnerOf(&fakeResolver{})
	r.CredentialResolver = func(authn.Keychain) imagecache.Resolver {
		return &fakeResolver{fail: map[string]bool{"nginx:1.25": true}}
	}
	r.RequireDigest = true
	_, err := settleModes(ctx, r, lab)
	var required *digestRequiredError
	if !stderrors.As(err, &required) || !strings.Contains(err.Error(), "nginx:1.25") || !strings.Contains(err.Error(), "IMAGE_DIGEST_REQUIRED") {
		t.Fatalf("%v", err)
	}
	if lab.Status.ImageCache != nil {
		t.Error("nothing is decided for a lab that failed")
	}
}

// A Lab written by hand cannot name another tenant's cache path or the platform's: only its own tenant's segment and snapshots pass.
func TestLabCannotNameAnotherTenantsCachePath(t *testing.T) {
	r := &LabReconciler{Mirror: imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}, AllowedRegistries: []string{"*"}}
	mk := func(img string) *laboratoryv1alpha1.Lab {
		return &laboratoryv1alpha1.Lab{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{names.LabelTenant: "acme"}},
			Spec:       laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "a", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: img}}},
		}
	}
	if err := r.validateRegistries(mk("localhost:5035/c/acme/docker.io/library/nginx:1")); err != nil {
		t.Fatalf("its own segment: %v", err)
	}
	for _, bad := range []string{"localhost:5035/c/other/docker.io/library/nginx:1", "LOCALHOST:5035/c/platform/ghcr.io/o/vpn:1", "localhost:5035/base:1"} {
		if err := r.validateRegistries(mk(bad)); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}
