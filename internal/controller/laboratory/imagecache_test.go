package laboratory

import (
	"context"
	"fmt"
	"testing"

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
