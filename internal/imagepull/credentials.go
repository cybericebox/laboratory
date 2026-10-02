package imagepull

import (
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// DockerConfigCredentials turns a kubernetes.io/dockerconfigjson document into the lookup
// Options.Credentials wants: the entry of the image's registry, nil (anonymous) when the
// document has none for it. An empty document means no credentials at all.
func DockerConfigCredentials(doc []byte) (func(ref string) *Auth, error) {
	if len(doc) == 0 {
		return func(string) *Auth { return nil }, nil
	}
	kc, err := imagecache.NewDockerConfigKeychain(doc)
	if err != nil {
		return nil, err
	}
	return func(ref string) *Auth {
		r, err := name.ParseReference(ref)
		if err != nil {
			return nil
		}
		a, err := kc.Resolve(r.Context())
		if err != nil || a == nil {
			return nil
		}
		cfg, err := a.Authorization()
		if err != nil || cfg == nil || cfg.Username == "" {
			return nil
		}
		return &Auth{Username: cfg.Username, Password: cfg.Password}
	}, nil
}
