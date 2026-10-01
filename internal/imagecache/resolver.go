package imagecache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Resolver turns an image reference into the digest its tag points at now.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (digest string, err error)
}

// RewritePinned is Rewrite for a reference pinned to digest: the tag is
// replaced by the digest, so every pod of a lab pulls the very same image.
// A reference the cache does not serve is returned unchanged.
func (r Rewriter) RewritePinned(ref, digest string) string {
	if digest == "" {
		return r.Rewrite(ref)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return r.Rewrite(ref)
	}
	pin := parsed.Context().Name() + "@" + digest
	if out := r.Rewrite(pin); out != pin {
		return out
	}
	return ref // not served by the cache
}

// DigestOf returns the digest of a reference that already names one, "" if not.
func DigestOf(ref string) string {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return ""
	}
	if d, ok := parsed.(name.Digest); ok {
		return d.DigestStr()
	}
	return ""
}

// RegistryResolver resolves tags with a HEAD request to the upstream registry
// (never through the cache, which would answer with what it has stored). Answers
// are remembered for TTL, so labs created in the same wave, and the prepull,
// see one digest even when the upstream tag moves meanwhile.
type RegistryResolver struct {
	Keychain authn.Keychain
	TTL      time.Duration
	// Timeout bounds one request; 15s when zero.
	Timeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Platforms lists the platforms of the nodes the images run on. With exactly
	// one, a multi-platform image is pinned to the manifest of that platform, so the
	// cache fetches that one image and not every architecture of the index. With
	// several (or none known) the index digest is pinned.
	Platforms func(ctx context.Context) []v1.Platform
	// get resolves a reference to a digest for a platform (nil: any); nil means the
	// real registry call. A field so tests need no registry.
	get func(ctx context.Context, ref name.Reference, kc authn.Keychain, platform *v1.Platform) (string, error)

	mu    sync.Mutex
	known map[string]pinned
}

type pinned struct {
	digest string
	at     time.Time
}

func (r *RegistryResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Resolve implements Resolver.
func (r *RegistryResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if d := DigestOf(ref); d != "" {
		return d, nil
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	var platform *v1.Platform
	if r.Platforms != nil {
		if ps := r.Platforms(ctx); len(ps) == 1 {
			platform = &ps[0]
		}
	}
	key := parsed.Name()
	if platform != nil {
		key += "|" + platform.OS + "/" + platform.Architecture
	}
	r.mu.Lock()
	if p, ok := r.known[key]; ok && r.now().Sub(p.at) < r.TTL {
		r.mu.Unlock()
		return p.digest, nil
	}
	r.mu.Unlock()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	get := r.get
	if get == nil {
		get = registryDigest
	}
	digest, err := get(ctx, parsed, r.Keychain, platform)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.known == nil {
		r.known = map[string]pinned{}
	}
	r.known[key] = pinned{digest: digest, at: r.now()}
	r.mu.Unlock()
	return digest, nil
}

// DockerConfigKeychain is a keychain over dockerconfigjson documents (the data
// of kubernetes.io/dockerconfigjson pull Secrets).
type DockerConfigKeychain struct {
	auths map[string]authn.AuthConfig
}

// NewDockerConfigKeychain parses dockerconfigjson documents; a malformed one is an error.
func NewDockerConfigKeychain(docs ...[]byte) (*DockerConfigKeychain, error) {
	k := &DockerConfigKeychain{auths: map[string]authn.AuthConfig{}}
	for _, doc := range docs {
		var cfg struct {
			Auths map[string]struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Auth     string `json:"auth"`
			} `json:"auths"`
		}
		if err := json.Unmarshal(doc, &cfg); err != nil {
			return nil, fmt.Errorf("dockerconfigjson: %w", err)
		}
		for host, a := range cfg.Auths {
			user, pass := a.Username, a.Password
			if user == "" && a.Auth != "" {
				raw, err := base64.StdEncoding.DecodeString(a.Auth)
				if err != nil {
					return nil, fmt.Errorf("dockerconfigjson auth of %s: %w", host, err)
				}
				user, pass, _ = strings.Cut(string(raw), ":")
			}
			if user != "" {
				k.auths[normalizeHost(host)] = authn.AuthConfig{Username: user, Password: pass}
			}
		}
	}
	return k, nil
}

// normalizeHost maps the forms a registry appears in (URL, index.docker.io) to one name.
func normalizeHost(h string) string {
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(strings.TrimSuffix(h, "/"), "/v1")
	switch h {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return h
}

// Resolve implements authn.Keychain.
func (k *DockerConfigKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if k != nil {
		if a, ok := k.auths[normalizeHost(res.RegistryStr())]; ok {
			return authn.FromConfig(a), nil
		}
	}
	return authn.Anonymous, nil
}

// registryDigest asks the registry for the digest of a reference. For an image
// index and a platform it is the digest of that platform's manifest; otherwise
// the digest of the reference itself.
func registryDigest(ctx context.Context, ref name.Reference, kc authn.Keychain, platform *v1.Platform) (string, error) {
	opts := []remote.Option{remote.WithContext(ctx)}
	if kc != nil {
		opts = append(opts, remote.WithAuthFromKeychain(kc))
	}
	if platform == nil {
		desc, err := remote.Head(ref, opts...)
		if err != nil {
			return "", err
		}
		return desc.Digest.String(), nil
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return "", err
	}
	if !desc.MediaType.IsIndex() {
		return desc.Digest.String(), nil
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return "", err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return "", err
	}
	for _, d := range m.Manifests {
		if d.Platform != nil && d.Platform.OS == platform.OS && d.Platform.Architecture == platform.Architecture && d.MediaType.IsImage() {
			return d.Digest.String(), nil
		}
	}
	return desc.Digest.String(), nil // no manifest for the platform: the index
}
