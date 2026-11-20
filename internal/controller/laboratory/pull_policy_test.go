package laboratory

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

const cachePrefix = "localhost:5035"

func TestSetPullPoliciesIsAlwaysOnCacheReferencesOnly(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: "a", Image: cachePrefix + "/c/acme/docker.io/library/nginx:1", ImagePullPolicy: corev1.PullIfNotPresent},
			{Name: "b", Image: "registry.example.com/x:1", ImagePullPolicy: corev1.PullIfNotPresent},
		},
		Containers: []corev1.Container{
			{Name: "c", Image: cachePrefix + "/c/acme/ghcr.io/o/app@sha256:" + strings.Repeat("a", 64), ImagePullPolicy: corev1.PullIfNotPresent},
			{Name: "d", Image: "other/app:1", ImagePullPolicy: corev1.PullNever},
		},
	}
	setPullPolicies(spec, cachePrefix)
	got := []corev1.PullPolicy{spec.InitContainers[0].ImagePullPolicy, spec.InitContainers[1].ImagePullPolicy, spec.Containers[0].ImagePullPolicy, spec.Containers[1].ImagePullPolicy}
	want := []corev1.PullPolicy{corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullAlways, corev1.PullNever}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("container %d: %s, want %s", i, got[i], want[i])
		}
	}
	if len(spec.ImagePullSecrets) != 0 {
		t.Errorf("never an image pull secret: %v", spec.ImagePullSecrets)
	}
	if pullPolicyFor("registry.example.com/x:1", cachePrefix) != corev1.PullIfNotPresent || pullPolicyFor("registry.example.com/x:1", "") != corev1.PullIfNotPresent {
		t.Error("an image that is not a cache reference keeps IfNotPresent")
	}
}

// A device pod: every container and init container with a cache reference pulls with Always, no pod has image pull secrets, and the
// reference carries the tenant segment (default without a label, the name when it is safe, the hashed form when it is not).
func TestDevicePodPullsAlwaysAndHasNoPullSecretAndTheTenantInItsReference(t *testing.T) {
	for label, segment := range map[string]string{"": "default", "acme": "acme", "Acme Corp": names.TenantSegment("Acme Corp"), "platform": names.TenantSegment("platform")} {
		d := bootstrapDevice() // the main container, the probe installer and the stager
		d.Spec.ImageMirror = cachePrefix
		d.Labels = map[string]string{}
		if label != "" {
			d.Labels[names.LabelTenant] = label
		}
		d.Spec.Interfaces = []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.2/24"}}}
		r := readinessReconciler()
		r.MirrorRegistries = imagecache.DefaultRegistries
		_, _, _, spec := r.workloadTemplate(d)
		if len(spec.InitContainers) < 2 {
			t.Fatalf("%q: the pod must have init containers: %d", label, len(spec.InitContainers))
		}
		if len(spec.ImagePullSecrets) != 0 {
			t.Errorf("%q: %v", label, spec.ImagePullSecrets)
		}
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			if !strings.HasPrefix(c.Image, cachePrefix+"/c/"+segment+"/") {
				t.Errorf("%q: container %s has %q, want the segment %q", label, c.Name, c.Image, segment)
			}
			if c.ImagePullPolicy != corev1.PullAlways {
				t.Errorf("%q: container %s pulls with %s", label, c.Name, c.ImagePullPolicy)
			}
		}
	}
	// A pod made without the cache keeps its policy.
	d := bootstrapDevice()
	_, _, _, spec := readinessReconciler().workloadTemplate(d)
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if c.ImagePullPolicy == corev1.PullAlways {
			t.Errorf("container %s of a pod without the cache must keep its policy", c.Name)
		}
	}
}

// The VPN and gateway pods use the segment "platform", pull with Always and carry no image pull secrets, with the operator's pull secrets
// configured or not; a Deployment of an earlier version that still has some is rolled to lose them.
func TestGroupPodsUseThePlatformSegmentAndHaveNoPullSecrets(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	r := &LabGroupReconciler{
		Client: c, LabBaseNetwork: "10.8.0.0/10", VPNImage: "ghcr.io/o/lab:v1", GatewayImage: "ghcr.io/o/lab:v1",
		Mirror: imagecache.Rewriter{Prefix: cachePrefix, Registries: imagecache.DefaultRegistries}, ImagePullSecrets: []string{"ghcr-pull"},
	}
	if err := r.ensureVPNDeployment(ctx, "ns", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayDeployment(ctx, "ns", false, nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"vpn", "gateway"} {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: n, Namespace: "ns"}, &d); err != nil {
			t.Fatal(err)
		}
		spec := d.Spec.Template.Spec
		if len(spec.ImagePullSecrets) != 0 {
			t.Errorf("%s: %v", n, spec.ImagePullSecrets)
		}
		for _, ctr := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			if !strings.HasPrefix(ctr.Image, cachePrefix+"/c/"+names.SegmentPlatform+"/ghcr.io/o/lab") || ctr.ImagePullPolicy != corev1.PullAlways {
				t.Errorf("%s: container %s has %q and %s", n, ctr.Name, ctr.Image, ctr.ImagePullPolicy)
			}
		}
	}
	// A Deployment of an earlier version.
	var d appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: "ns"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "platform-cache-registry"}}
	if !r.convergeGroupPod(ctx, "ns", &d, names.ComponentVPN, r.VPNImage) {
		t.Fatal("a pod with a pull secret must be rolled")
	}
	if len(d.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("%v", d.Spec.Template.Spec.ImagePullSecrets)
	}
}
