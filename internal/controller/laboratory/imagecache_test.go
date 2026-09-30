package laboratory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

func cachedLabReconciler(t *testing.T, cache bool, objs ...client.Object) *LabReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(pruneScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}).Build()
	r := &LabReconciler{Client: c}
	if cache {
		r.Mirror = imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
	}
	return r
}

func TestImageCacheModeIsFixedAtCreation(t *testing.T) {
	ctx := context.Background()
	lab := newLab("new")
	r := cachedLabReconciler(t, true, lab)
	if updated, err := r.ensureModes(ctx, lab); err != nil || !updated {
		t.Fatalf("first reconcile must stamp the lab: %v %v", updated, err)
	}
	if lab.Status.ImageCache == nil || !*lab.Status.ImageCache {
		t.Fatal("a lab created with the cache on uses it")
	}
	if got := r.deviceMirror(lab, laboratoryv1alpha1.DeviceTypeContainer); got != "localhost:5035" {
		t.Fatalf("device mirror %q", got)
	}
	if r.deviceMirror(lab, laboratoryv1alpha1.DeviceTypeHub) != "" {
		t.Fatal("a switch pulls no image")
	}
	r.Mirror = imagecache.Rewriter{}
	if _, err := r.ensureModes(ctx, lab); err != nil || !*lab.Status.ImageCache {
		t.Fatal("switching the cache off must not change a stamped lab")
	}

	off := newLab("off")
	r2 := cachedLabReconciler(t, false, off)
	if _, err := r2.ensureModes(ctx, off); err != nil {
		t.Fatal(err)
	}
	r2.Mirror = imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
	if _, err := r2.ensureModes(ctx, off); err != nil || *off.Status.ImageCache || r2.deviceMirror(off, laboratoryv1alpha1.DeviceTypeContainer) != "" {
		t.Fatal("a lab created with the cache off stays off after it is switched on")
	}
}

func TestImageCacheLabWithDevicesIsNotSwitched(t *testing.T) {
	legacy := newLab("legacy")
	dev := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{
		Name: "legacy-web", Namespace: "ns", Labels: map[string]string{names.LabelLab: "legacy"},
	}}
	r := cachedLabReconciler(t, true, legacy, dev)
	if _, err := r.ensureModes(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	if *legacy.Status.ImageCache || *legacy.Status.StatePersistence {
		t.Fatal("a lab that predates the switches keeps pulling directly")
	}
}

func TestClassImagesFollowTheLabMode(t *testing.T) {
	mk := func(cached bool) *laboratoryv1alpha1.Lab {
		l := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
			{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:1"},
		}}}
		l.Status.ImageCache = &cached
		return l
	}
	rw := imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
	got := classImages([]*laboratoryv1alpha1.Lab{mk(true), mk(false)}, rw)
	want := []string{"localhost:5035/docker.io/library/nginx:1", "nginx:1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

var _ = Describe("Image cache: device image rewrite", func() {
	ctx := context.Background()
	mk := func(ns, name, mirror string) (*DeviceReconciler, reconcile.Request) {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		dev := &laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{
				Type: laboratoryv1alpha1.DeviceTypeContainer, Name: "web", LabRef: "lab", Image: "nginx:1.25", ImageMirror: mirror,
				Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{
					Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.5/24"}}},
			},
		}
		Expect(k8sClient.Create(ctx, dev)).To(Succeed())
		r := &DeviceReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), NetConfigImage: "ghcr.io/cybericebox/laboratory-node:v1",
			MirrorRegistries: imagecache.DefaultRegistries,
		}
		return r, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	}

	It("pulls device and netconfig images through the cache when the device was created for it", func() {
		r, req := mk("cache-on", "lab-web", "localhost:5035")
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, req.NamespacedName, &dep)).To(Succeed())
		spec := dep.Spec.Template.Spec
		Expect(spec.Containers[0].Image).To(Equal("localhost:5035/docker.io/library/nginx:1.25"))
		Expect(spec.InitContainers).To(HaveLen(1))
		Expect(spec.InitContainers[0].Image).To(Equal("localhost:5035/ghcr.io/cybericebox/laboratory-node:v1"))
	})

	It("pulls the digest the lab was pinned to, and the tag when nothing was pinned", func() {
		r, req := mk("cache-pin", "lab-web", "localhost:5035")
		var d laboratoryv1alpha1.Device
		Expect(k8sClient.Get(ctx, req.NamespacedName, &d)).To(Succeed())
		d.Spec.ImageDigests = map[string]string{"nginx:1.25": digA}
		Expect(k8sClient.Update(ctx, &d)).To(Succeed())
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, req.NamespacedName, &dep)).To(Succeed())
		spec := dep.Spec.Template.Spec
		Expect(spec.Containers[0].Image).To(Equal("localhost:5035/docker.io/library/nginx@" + digA))
		Expect(spec.InitContainers[0].Image).To(Equal("localhost:5035/ghcr.io/cybericebox/laboratory-node:v1"), "no digest for the netconfig image: fall back to its tag")
	})

	It("leaves the images alone for a device created without the cache", func() {
		r, req := mk("cache-off", "lab-web", "")
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, req.NamespacedName, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.25"))
		Expect(dep.Spec.Template.Spec.InitContainers[0].Image).To(Equal("ghcr.io/cybericebox/laboratory-node:v1"))
	})

	It("starts a snapshot-backed pod from the cached base image and the raw snapshot image", func() {
		r, req := mk("cache-state", "lab-web", "localhost:5035")
		var d laboratoryv1alpha1.Device
		Expect(k8sClient.Get(ctx, req.NamespacedName, &d)).To(Succeed())
		d.Spec.State = &laboratoryv1alpha1.DeviceStateSpec{Enabled: true}
		Expect(k8sClient.Update(ctx, &d)).To(Succeed())
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "cache-state", Name: "lab-web-1"}, &pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Image).To(Equal("localhost:5035/docker.io/library/nginx:1.25"))
	})
})

type fakeResolver struct {
	digests map[string]string
	fail    map[string]bool
	calls   []string
}

func (f *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	f.calls = append(f.calls, ref)
	if f.fail[ref] {
		return "", fmt.Errorf("upstream unreachable")
	}
	return f.digests[ref], nil
}

const (
	digA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digN = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func pinLab() *laboratoryv1alpha1.Lab {
	l := newLab("pin")
	l.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx:1.25"},
		{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "registry.example.com/team/db:1"},
		{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeHub},
	}
	return l
}

func TestLabPinsImagesToDigestsAtCreation(t *testing.T) {
	ctx := context.Background()
	lab := pinLab()
	res := &fakeResolver{digests: map[string]string{"nginx:1.25": digA, "ghcr.io/cybericebox/laboratory-node:v1": digN}}
	r := cachedLabReconciler(t, true, lab)
	r.Resolver, r.NetConfigImage = res, "ghcr.io/cybericebox/laboratory-node:v1"
	if _, err := r.ensureModes(ctx, lab); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"nginx:1.25": digA, "ghcr.io/cybericebox/laboratory-node:v1": digN}
	if fmt.Sprint(lab.Status.ImageDigests) != fmt.Sprint(want) || lab.Status.ImageWarning != "" {
		t.Fatalf("digests %v warning %q", lab.Status.ImageDigests, lab.Status.ImageWarning)
	}
	for _, c := range res.calls {
		if c == "registry.example.com/team/db:1" {
			t.Fatal("an image of a registry the cache does not serve is pulled directly and never resolved")
		}
	}
	web := r.deviceDigests(lab, lab.Spec.Devices[0])
	if fmt.Sprint(web) != fmt.Sprint(want) {
		t.Fatalf("web device digests %v", web)
	}
	// An image the cache does not serve stays unpinned; the netconfig image is pinned.
	if got := r.deviceDigests(lab, lab.Spec.Devices[1]); fmt.Sprint(got) != fmt.Sprint(map[string]string{"ghcr.io/cybericebox/laboratory-node:v1": digN}) {
		t.Fatalf("db device digests: %v", got)
	}

	// Stamped once: a later reconcile never resolves again.
	res.calls = nil
	res.digests["nginx:1.25"] = "sha256:moved"
	if _, err := r.ensureModes(ctx, lab); err != nil || len(res.calls) != 0 || lab.Status.ImageDigests["nginx:1.25"] != digA {
		t.Fatalf("pins are fixed at creation: %v %v", err, res.calls)
	}
}

func TestLabFallsBackToTagsAndWarnsWhenResolutionFails(t *testing.T) {
	lab := pinLab()
	res := &fakeResolver{digests: map[string]string{"ghcr.io/cybericebox/laboratory-node:v1": digN}, fail: map[string]bool{"nginx:1.25": true}}
	r := cachedLabReconciler(t, true, lab)
	r.Resolver, r.NetConfigImage = res, "ghcr.io/cybericebox/laboratory-node:v1"
	if _, err := r.ensureModes(context.Background(), lab); err != nil {
		t.Fatal(err)
	}
	if _, pinned := lab.Status.ImageDigests["nginx:1.25"]; pinned {
		t.Fatal("an unresolved image must not get a digest")
	}
	if lab.Status.ImageDigests["ghcr.io/cybericebox/laboratory-node:v1"] != digN {
		t.Fatal("the other images are still pinned")
	}
	if !strings.Contains(lab.Status.ImageWarning, "nginx:1.25") || !strings.Contains(lab.Status.ImageWarning, "pulled by tag") {
		t.Fatalf("warning %q", lab.Status.ImageWarning)
	}
	if lab.Status.ImageCache == nil || !*lab.Status.ImageCache {
		t.Fatal("the lab still uses the cache, by tag")
	}
}

func TestNoPinningWithoutCacheOrResolver(t *testing.T) {
	lab := pinLab()
	res := &fakeResolver{}
	r := cachedLabReconciler(t, false, lab)
	r.Resolver = res
	if _, err := r.ensureModes(context.Background(), lab); err != nil || len(res.calls) != 0 || lab.Status.ImageDigests != nil {
		t.Fatalf("cache off: nothing is resolved: %v %v", err, res.calls)
	}
	lab2 := pinLab()
	r2 := cachedLabReconciler(t, true, lab2)
	if _, err := r2.ensureModes(context.Background(), lab2); err != nil || lab2.Status.ImageDigests != nil || lab2.Status.ImageWarning != "" {
		t.Fatal("no resolver configured: tags are used without a warning")
	}
}

func TestClassImagesUsePinnedDigests(t *testing.T) {
	l := pinLab()
	cached := true
	l.Status.ImageCache = &cached
	l.Status.ImageDigests = map[string]string{"nginx:1.25": digA}
	rw := imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
	got := classImages([]*laboratoryv1alpha1.Lab{l}, rw)
	want := []string{"localhost:5035/docker.io/library/nginx@" + digA, "registry.example.com/team/db:1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestGroupImagePinning(t *testing.T) {
	rw := imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
	g := &LabGroupReconciler{Mirror: rw, Resolver: &fakeResolver{digests: map[string]string{"ghcr.io/cybericebox/laboratory-lab:v1": digA}}}
	ctx := context.Background()
	if got := g.cachedImage(ctx, "ghcr.io/cybericebox/laboratory-lab:v1"); got != "localhost:5035/ghcr.io/cybericebox/laboratory-lab@"+digA {
		t.Fatal(got)
	}
	g.Resolver = &fakeResolver{fail: map[string]bool{"ghcr.io/cybericebox/laboratory-lab:v1": true}}
	if got := g.cachedImage(ctx, "ghcr.io/cybericebox/laboratory-lab:v1"); got != "localhost:5035/ghcr.io/cybericebox/laboratory-lab:v1" {
		t.Fatalf("a failed resolution falls back to the tag, got %s", got)
	}
	if got := (&LabGroupReconciler{}).cachedImage(ctx, "ghcr.io/x/y:1"); got != "ghcr.io/x/y:1" {
		t.Fatalf("cache off: %s", got)
	}
}

func TestPullKeychainReadsOperatorPullSecrets(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "laboratory-system"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"gh","password":"tok"}}}`)},
	}
	c := fake.NewClientBuilder().WithScheme(retentionScheme(t)).WithObjects(sec).Build()
	k := &PullKeychain{Reader: c, Namespace: "laboratory-system", Names: []string{"missing", "pull"}}
	ref, _ := name.ParseReference("ghcr.io/o/a:1")
	a, err := k.Resolve(ref.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := a.Authorization(); cfg.Username != "gh" || cfg.Password != "tok" {
		t.Fatalf("%+v", cfg)
	}
	other, _ := name.ParseReference("quay.io/o/a:1")
	a, _ = k.Resolve(other.Context())
	if cfg, _ := a.Authorization(); cfg.Username != "" {
		t.Fatal("a registry without credentials is asked anonymously")
	}
}
