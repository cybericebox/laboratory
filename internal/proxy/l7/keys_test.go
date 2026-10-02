package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func pubPEM(pub ed25519.PublicKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestSecretKeysAndGroupTenant(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	k1, _, _ := ed25519.GenerateKey(rand.Reader)
	k2, _, _ := ed25519.GenerateKey(rand.Reader)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: names.AccessKeysNamespace, Name: "tenant-acme-access-keys"},
			Data: map[string][]byte{"k1": pubPEM(k1), "k2": pubPEM(k2), "junk": []byte("not a key")}},
		// A Secret of the same name in another namespace is not a source of keys.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "tenant-evil-access-keys"}, Data: map[string][]byte{"k": pubPEM(k1)}},
		&laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}},
		&laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: names.EncodeName("Team One"), Labels: map[string]string{names.LabelTenant: "other"}}},
		&laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}},
	).Build()
	keys := SecretKeys(c)
	if got, ok := keys("acme", "k1"); !ok || !got.Equal(k1) {
		t.Fatal("k1")
	}
	if got, ok := keys("acme", "k2"); !ok || !got.Equal(k2) {
		t.Fatal("k2")
	}
	for _, miss := range [][2]string{{"acme", "junk"}, {"acme", "ghost"}, {"nobody", "k1"}, {"evil", "k"}} {
		if _, ok := keys(miss[0], miss[1]); ok {
			t.Errorf("%v must not resolve", miss)
		}
	}
	gt := LabGroupTenant(c)
	for group, want := range map[string]string{"g1": "acme", "Team One": "other", "legacy": names.DefaultTenant} {
		if got, ok := gt(group); !ok || got != want {
			t.Errorf("group %q: %q %v", group, got, ok)
		}
	}
	if _, ok := gt("missing"); ok {
		t.Error("an unknown group has no tenant")
	}
}
