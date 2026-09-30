package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cybericebox/laboratory/internal/names"
)

func regSecret(name string, typ corev1.SecretType, data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: names.SystemNamespace},
		Type:       typ,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(data)},
	}
}

func TestCopyPullSecrets(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	ctx := context.Background()
	src := regSecret("regcred", corev1.SecretTypeDockerConfigJson, `{"auths":{}}`)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(src).Build()

	if err := copyPullSecrets(ctx, c, []string{"regcred"}, "team-alpha"); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: "regcred", Namespace: "team-alpha"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != corev1.SecretTypeDockerConfigJson || string(got.Data[corev1.DockerConfigJsonKey]) != `{"auths":{}}` {
		t.Fatalf("copy: %+v", got)
	}

	// the copy follows the source
	src.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"r":{}}}`)
	if err := c.Update(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := copyPullSecrets(ctx, c, []string{"regcred"}, "team-alpha"); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, types.NamespacedName{Name: "regcred", Namespace: "team-alpha"}, &got)
	if string(got.Data[corev1.DockerConfigJsonKey]) != `{"auths":{"r":{}}}` {
		t.Fatalf("copy did not follow the source: %s", got.Data[corev1.DockerConfigJsonKey])
	}
}

func TestCopyPullSecretsRefusesMissingOrWrongType(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(regSecret("opaque", corev1.SecretTypeOpaque, "x")).Build()
	if err := copyPullSecrets(context.Background(), c, []string{"absent"}, "team-alpha"); err == nil {
		t.Fatal("a missing source must be an error")
	}
	if err := copyPullSecrets(context.Background(), c, []string{"opaque"}, "team-alpha"); err == nil {
		t.Fatal("a non-registry Secret must be an error")
	}
	if err := copyPullSecrets(context.Background(), c, nil, "team-alpha"); err != nil {
		t.Fatalf("nothing configured is fine: %v", err)
	}
}

func TestPullSecretRefs(t *testing.T) {
	if pullSecretRefs(nil) != nil {
		t.Fatal("no secrets, no refs")
	}
	refs := pullSecretRefs([]string{"a", "b"})
	if len(refs) != 2 || refs[0].Name != "a" || refs[1].Name != "b" {
		t.Fatalf("refs: %v", refs)
	}
}
