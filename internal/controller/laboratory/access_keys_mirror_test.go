package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
	"github.com/cybericebox/laboratory/internal/names"
)

const mirrorNS = "laboratory-device-access"

func accessKeySecret(tenant string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: names.AccessKeysNamespace, Name: names.AccessKeysSecret(tenant)}, Data: data}
}

func mirrorData(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: mirrorNS, Name: names.AccessKeysConfigMap}, &cm); err != nil {
		t.Fatalf("the mirror ConfigMap: %v", err)
	}
	return cm.Data
}

func TestAccessKeysMirrorCopiesEveryKeyAndFollowsTheSecrets(t *testing.T) {
	k1, k2, k3 := granttest.NewKey(t, "acme", "k1"), granttest.NewKey(t, "acme", "k2"), granttest.NewKey(t, "beta", "key.2026")
	acme := accessKeySecret("acme", map[string][]byte{"k1": k1.PEM(t), "k2": k2.PEM(t)})
	beta := accessKeySecret("beta", map[string][]byte{"key.2026": k3.PEM(t)})
	c := fake.NewClientBuilder().WithScheme(bwScheme()).WithObjects(acme, beta).Build()
	m := &AccessKeysMirror{Client: c, Reader: c, Namespace: mirrorNS}
	ctx := context.Background()
	if _, err := m.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	got := mirrorData(t, c)
	if len(got) != 3 || got["acme.k1"] != string(k1.PEM(t)) || got["acme.k2"] != string(k2.PEM(t)) || got["beta.key.2026"] != string(k3.PEM(t)) {
		t.Fatalf("mirror = %v", got)
	}

	// RemoveAccessKey: the key leaves the Secret and leaves the mirror in the same pass
	acme.Data = map[string][]byte{"k2": k2.PEM(t)}
	if err := c.Update(ctx, acme); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := mirrorData(t, c); len(got) != 2 || got["acme.k1"] != "" {
		t.Fatalf("a removed key stayed in the mirror: %v", got)
	}
	// a tenant deleted
	if err := c.Delete(ctx, beta); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := mirrorData(t, c); len(got) != 1 || got["acme.k2"] == "" {
		t.Fatalf("a deleted tenant stayed in the mirror: %v", got)
	}
}

// Only what the proxy would accept as a key is mirrored, and nothing in a Secret of that namespace that is not one.
func TestAccessKeysMirrorCarriesOnlyPublicKeysOfTenants(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	secrets := []*corev1.Secret{
		accessKeySecret("acme", map[string][]byte{
			"k1":                                 k.PEM(t),
			"junk":                               []byte("not a key"),
			"empty":                              nil,
			"a/b":                                k.PEM(t),
			".":                                  k.PEM(t),
			"..":                                 k.PEM(t),
			"k with ":                            k.PEM(t),
			"toolong" + string(make([]byte, 70)): k.PEM(t),
		}),
		{ObjectMeta: metav1.ObjectMeta{Namespace: names.AccessKeysNamespace, Name: "something-else"}, Data: map[string][]byte{"k1": k.PEM(t)}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: names.AccessKeysNamespace, Name: "tenant--access-keys"}, Data: map[string][]byte{"k1": k.PEM(t)}},
		accessKeySecret("Bad_Tenant", map[string][]byte{"k1": k.PEM(t)}),
	}
	objs := make([]client.Object, len(secrets))
	for i, s := range secrets {
		objs[i] = s
	}
	c := fake.NewClientBuilder().WithScheme(bwScheme()).WithObjects(objs...).Build()
	if _, err := (&AccessKeysMirror{Client: c, Reader: c, Namespace: mirrorNS}).Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	got := mirrorData(t, c)
	if len(got) != 1 || got["acme.k1"] != string(k.PEM(t)) {
		t.Fatalf("mirror = %v", got)
	}
}

// A pass writes only when something differs, and a hand edit of the ConfigMap is put back.
func TestAccessKeysMirrorWritesOnlyWhatDiffers(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	c := fake.NewClientBuilder().WithScheme(bwScheme()).WithObjects(accessKeySecret("acme", map[string][]byte{"k1": k.PEM(t)})).Build()
	m := &AccessKeysMirror{Client: c, Reader: c, Namespace: mirrorNS}
	ctx := context.Background()
	if _, err := m.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: mirrorNS, Name: names.AccessKeysConfigMap}
	_ = c.Get(ctx, key, &cm)
	rv := cm.ResourceVersion
	if _, err := m.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, key, &cm)
	if cm.ResourceVersion != rv {
		t.Fatal("an unchanged mirror was written again")
	}
	cm.Data["mallory.k1"] = string(k.PEM(t))
	if err := c.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := mirrorData(t, c); len(got) != 1 || got["acme.k1"] == "" {
		t.Fatalf("a key added by hand stayed: %v", got)
	}
}

func TestAccessKeysSecretTenantIsTheInverseOfTheName(t *testing.T) {
	for _, tenant := range []string{"acme", "a", "default", "x-y-z"} {
		if got, ok := names.AccessKeysSecretTenant(names.AccessKeysSecret(tenant)); !ok || got != tenant {
			t.Errorf("%q -> %q %v", tenant, got, ok)
		}
	}
	for _, name := range []string{"", "tenant-", "tenant--access-keys", "tenant-acme", "acme-access-keys", "tenant-acme-access-keysx"} {
		if got, ok := names.AccessKeysSecretTenant(name); ok {
			t.Errorf("%q is no access keys Secret, got %q", name, got)
		}
	}
}

// The pod of a device carries the name of its LabGroup (the device access service binds a console grant to it), made by the operator.
func TestDevicePodCarriesTheGroupName(t *testing.T) {
	scheme := bwScheme()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lg-ns", Labels: map[string]string{names.LabelGroup: "grp-1"}}}
	group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "grp-1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, group).Build()
	r := &DeviceReconciler{Client: c, Reader: c}
	device := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Namespace: "lg-ns", UID: "device-uid", Labels: map[string]string{names.LabelGroupName: "forged"}}}
	device.Spec.Name, device.Spec.LabRef = "web", "lab"
	labels, selector, _, _ := r.workloadTemplate(device)
	if _, ok := labels[names.LabelGroupName]; ok {
		t.Fatalf("a user label set the group name: %v", labels)
	}
	if err := r.stampGroupName(context.Background(), device, labels); err != nil {
		t.Fatal(err)
	}
	if labels[names.LabelGroupName] != "grp-1" {
		t.Fatalf("labels = %v", labels)
	}
	if _, ok := selector[names.LabelGroupName]; ok {
		t.Fatalf("the group name leaked into the selector: %v", selector)
	}
	// a namespace with no LabGroup gets no label, so no console opens in it
	device.Namespace = "elsewhere"
	labels, _, _, _ = r.workloadTemplate(device)
	if err := r.stampGroupName(context.Background(), device, labels); err != nil {
		t.Fatal(err)
	}
	if _, ok := labels[names.LabelGroupName]; ok {
		t.Fatalf("a device outside a group got the label: %v", labels)
	}
}
