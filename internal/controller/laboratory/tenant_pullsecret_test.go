package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func secretScheme(t *testing.T) *runtime.Scheme {
	s := pruneScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func tenantWithPullSecret(name, secret string) *laboratoryv1alpha1.Tenant {
	return &laboratoryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       laboratoryv1alpha1.TenantSpec{Images: laboratoryv1alpha1.TenantImages{PullSecret: secret}},
	}
}

func tenantRegSecret(name string, typ corev1.SecretType, data string) *corev1.Secret {
	s := regSecret(name, typ, data)
	s.Namespace = names.TenantsNamespace
	return s
}
