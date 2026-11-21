package laboratory

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
)

func regSecret(name string, typ corev1.SecretType, data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: names.SystemNamespace},
		Type:       typ,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(data)},
	}
}
