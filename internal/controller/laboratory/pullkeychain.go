package laboratory

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// PullKeychain authenticates requests to upstream registries with the
// operator's image pull Secrets (kubernetes.io/dockerconfigjson in Namespace).
// The Secrets are read on every resolution, so a rotated credential is used at once.
type PullKeychain struct {
	Reader    client.Reader
	Namespace string
	Names     []string
}

// Resolve implements authn.Keychain. A missing or unreadable Secret means
// anonymous access, never a failure: public images need no credentials.
func (k *PullKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	var docs [][]byte
	for _, name := range k.Names {
		var s corev1.Secret
		if err := k.Reader.Get(context.Background(), types.NamespacedName{Namespace: k.Namespace, Name: name}, &s); err != nil {
			continue
		}
		if doc := s.Data[corev1.DockerConfigJsonKey]; len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	kc, err := imagecache.NewDockerConfigKeychain(docs...)
	if err != nil {
		return authn.Anonymous, nil
	}
	return kc.Resolve(res)
}
