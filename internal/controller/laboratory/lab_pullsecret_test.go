package laboratory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

func labWithPullSecret(name, ns, secret string, labels map[string]string) *laboratoryv1alpha1.Lab {
	return &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec:       laboratoryv1alpha1.LabSpec{ImagePullSecret: secret},
	}
}

func labRegSecret(name, ns string) *corev1.Secret {
	s := regSecret(name, corev1.SecretTypeDockerConfigJson, `{"auths":{"ghcr.io":{}}}`)
	s.Namespace = ns
	return s
}

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

func pullSpec() *corev1.PodSpec {
	return &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "i", ImagePullPolicy: corev1.PullIfNotPresent}},
		Containers:     []corev1.Container{{Name: "c", ImagePullPolicy: corev1.PullIfNotPresent}},
	}
}

func refNames(refs []corev1.LocalObjectReference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

// A lab with its own registry credentials gives every device pod its Secret, next to the
// tenant's when the tenant has one, and every container asks the registry on each start.
func TestDevicePodOfALabWithItsOwnPullSecret(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(
		tenantWithPullSecret("acme", "acme-registry"), tenantWithPullSecret("plain", ""),
		labWithPullSecret("l1", "team", "l1-registry", nil), labRegSecret("l1-registry", "team"),
		labWithPullSecret("l2", "team", "", nil),
	).Build()
	r := &DeviceReconciler{Client: c}

	for _, tc := range []struct {
		lab, tenant string
		want        []string
		always      bool
	}{
		{"l1", "plain", []string{"l1-registry"}, true},
		{"l1", "acme", []string{names.TenantPullSecret, "l1-registry"}, true},
		{"l2", "plain", []string{}, false},
		{"l2", "acme", []string{names.TenantPullSecret}, true},
	} {
		spec := pullSpec()
		if err := r.applyPullSecrets(ctx, labDevice(tc.lab, "team", tc.tenant), spec); err != nil {
			t.Fatalf("%s/%s: %v", tc.lab, tc.tenant, err)
		}
		if got := refNames(spec.ImagePullSecrets); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s/%s: pull secrets %v, want %v", tc.lab, tc.tenant, got, tc.want)
		}
		want := corev1.PullIfNotPresent
		if tc.always {
			want = corev1.PullAlways
		}
		if spec.Containers[0].ImagePullPolicy != want || spec.InitContainers[0].ImagePullPolicy != want {
			t.Errorf("%s/%s: pull policy %s/%s, want %s", tc.lab, tc.tenant, spec.InitContainers[0].ImagePullPolicy, spec.Containers[0].ImagePullPolicy, want)
		}
	}
}

// The agent creates the lab's pull Secret right after the Lab: until it is there, the
// device waits and retries; it is never a failure and no pod starts without it.
func TestDeviceWaitsForTheLabsPullSecret(t *testing.T) {
	ctx := context.Background()
	s := secretScheme(t)
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	dev := labDevice("l1", "team", "")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(labWithPullSecret("l1", "team", "l1-registry", nil), dev).
		WithStatusSubresource(&laboratoryv1alpha1.Device{}).Build()
	r := &DeviceReconciler{Client: c, Scheme: s}

	if err := r.applyPullSecrets(ctx, dev, pullSpec()); !errors.Is(err, errPullSecretPending) {
		t.Fatalf("a missing lab pull Secret is pending: %v", err)
	}
	res, err := r.reconcileWorkload(ctx, dev)
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("a missing pull Secret is retried, not failed: %+v %v", res, err)
	}
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps, client.InNamespace("team")); err != nil || len(deps.Items) != 0 {
		t.Fatalf("no workload without its pull Secret: %d %v", len(deps.Items), err)
	}

	if err := c.Create(ctx, labRegSecret("l1-registry", "team")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileWorkload(ctx, dev); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team", Name: workloadName(dev)}, &dep); err != nil {
		t.Fatal(err)
	}
	if got := refNames(dep.Spec.Template.Spec.ImagePullSecrets); fmt.Sprint(got) != "[l1-registry]" {
		t.Fatalf("pull secrets %v", got)
	}
}

// The same wait for a snapshot-backed device: its incarnation does not move while it waits.
func TestStateDeviceWaitsForTheLabsPullSecret(t *testing.T) {
	ctx := context.Background()
	s := secretScheme(t)
	dev := labDevice("l1", "team", "")
	dev.Spec.State = &laboratoryv1alpha1.DeviceStateSpec{Enabled: true}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(labWithPullSecret("l1", "team", "l1-registry", nil), dev).
		WithStatusSubresource(&laboratoryv1alpha1.Device{}).Build()
	r := &DeviceReconciler{Client: c, Scheme: s}

	res, err := r.reconcilePod(ctx, dev)
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("a missing pull Secret is retried, not failed: %+v %v", res, err)
	}
	var cur laboratoryv1alpha1.Device
	if err := c.Get(ctx, client.ObjectKeyFromObject(dev), &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Status.State != nil && cur.Status.State.Incarnation != 0 {
		t.Fatalf("the incarnation moved while waiting: %d", cur.Status.State.Incarnation)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace("team")); err != nil || len(pods.Items) != 0 {
		t.Fatalf("no pod without its pull Secret: %d %v", len(pods.Items), err)
	}
}

// A lab with its own pull secret pulls straight from its registry, like a tenant with one:
// the shared cache would fetch its private images with the platform's credentials.
func TestLabWithOwnPullSecretBypassesTheImageCache(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		secret string
		cache  bool
	}{{"l-registry", false}, {"", true}} {
		lab := newLab("l")
		lab.Spec.ImagePullSecret = tc.secret
		r := cachedLabReconciler(t, true, lab)
		r.Mirror = imagecache.Rewriter{Prefix: "localhost:5035", Registries: imagecache.DefaultRegistries}
		if _, err := r.ensureModes(ctx, lab); err != nil {
			t.Fatal(err)
		}
		if *lab.Status.ImageCache != tc.cache {
			t.Errorf("secret %q: cache=%v, want %v", tc.secret, *lab.Status.ImageCache, tc.cache)
		}
		if !tc.cache && len(lab.Status.ImageDigests) != 0 {
			t.Errorf("no pinning through the platform keychain: %v", lab.Status.ImageDigests)
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
	if s := f.labStatus("c"); s != nil && s.Reason == laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("lab c waits for a prepull: %+v", s)
	}
	if s := f.labStatus("a"); s != nil && s.Reason == laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("lab a waits for its group's prepull: %+v", s)
	}
}
