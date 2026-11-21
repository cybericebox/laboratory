package laboratory

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

func labDevice(lab, ns, tenant string) *laboratoryv1alpha1.Device {
	d := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: lab + "-web", Namespace: ns, Labels: map[string]string{}},
		Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: lab, Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "ghcr.io/acme/web@sha256:" + fmt.Sprintf("%064d", 1)},
	}
	if tenant != "" {
		d.Labels[names.LabelTenant] = tenant
	}
	return d
}

// A lab with its own pull secret goes through the cache like every other: the gate proves each pull with the pull's own credentials, so
// the shared cache never fetches its private images with anyone else's. (Its tags are pinned with those credentials: see
// TestLabWithCredentialsIsPinnedWithItsOwnCredentials.)
func TestLabWithOwnPullSecretUsesTheImageCache(t *testing.T) {
	ctx := context.Background()
	for _, secret := range []string{"l-registry", ""} {
		lab := newLab("l")
		lab.Spec.ImagePullSecret = secret
		r := cachedLabReconciler(t, true, lab)
		r.Mirror = imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
		if _, err := r.ensureModes(ctx, lab); err != nil {
			t.Fatal(err)
		}
		if !*lab.Status.ImageCache {
			t.Errorf("secret %q: every lab goes through the cache when it is on", secret)
		}
	}
}

// The node prepull pulls with the tenant's credentials or none: a lab with its own pull secret
// is left out, both as its own class and from its group's request, and does not wait for it.
func TestSchedulerDoesNotPrepullALabWithItsOwnPullSecret(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 10
	cfg.Prepull = true
	f := newSchedFixture(t, cfg)
	f.s.LabNodeSelector = map[string]string{"pool": "labs"}
	f.addLabNode("n1")
	f.addLab("a", "g1", nil, "web")
	f.addLab("b", "g1", nil, "web")
	f.addLab("c", "", nil, "web")
	for _, name := range []string{"a", "c"} {
		var lab laboratoryv1alpha1.Lab
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + name, Name: name}, &lab); err != nil {
			t.Fatal(err)
		}
		lab.Spec.ImagePullSecret = name + "-registry"
		if err := f.c.Update(context.Background(), &lab); err != nil {
			t.Fatal(err)
		}
	}

	f.tick()
	ip := f.imagePull("g/g1")
	if ip == nil {
		t.Fatal("the group still prepulls its other labs")
	}
	if fmt.Sprint(ip.Spec.Images) != "[reg/b-web]" {
		t.Fatalf("group images %v", ip.Spec.Images)
	}
	var ips laboratoryv1alpha1.ImagePullList
	if err := f.c.List(context.Background(), &ips); err != nil || len(ips.Items) != 1 {
		t.Fatalf("only the group's request: %d %v", len(ips.Items), err)
	}
	// Lab c was dispatched at once; a and b are one gang and wait together for the prepull of their group.
	f.wantStates("a/web=Q b/web=Q c/web=S")
	if s := f.labStatus("c"); s != nil && s.Reason == laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("lab c waits for a prepull: %+v", s)
	}
	if s := f.labStatus("a"); s == nil || s.Reason != laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("lab a waits with its gang for the group's prepull: %+v", s)
	}
}
