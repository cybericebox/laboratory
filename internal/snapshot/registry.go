package snapshot

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Latest is the tag every snapshot of a device is pushed under; the status
// records the digest, so older manifests are simply untagged and left to the
// registry's garbage collection.
const Latest = "latest"

// Registry is a client of the snapshot registry (zot) with write access.
type Registry struct {
	// Host is host:port of the registry, e.g. "localhost:5035". Hosts on the
	// loopback or a private network are spoken to over plain HTTP.
	Host string
	Auth authn.Authenticator
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

func (r *Registry) opts(ctx context.Context) []remote.Option {
	o := []remote.Option{remote.WithContext(ctx)}
	if r.Auth != nil {
		o = append(o, remote.WithAuth(r.Auth))
	}
	if r.Transport != nil {
		o = append(o, remote.WithTransport(r.Transport))
	}
	return o
}

func (r *Registry) repo(repo string) (name.Repository, error) {
	return name.NewRepository(r.Host+"/"+repo, name.Insecure)
}

// Ref is the pullable reference of a pushed manifest.
func (r *Registry) Ref(repo string, digest v1.Hash) string {
	return fmt.Sprintf("%s/%s@%s", r.Host, repo, digest)
}

// Push makes img available as repo:latest. The base layers of img (the first
// baseLayers) are placed in the shared base repository once and mounted into
// repo, so a device's first snapshot does not upload the whole base image.
// It returns the pullable reference by digest.
func (r *Registry) Push(ctx context.Context, repo string, img v1.Image, baseLayers int) (string, v1.Hash, error) {
	target, err := r.repo(repo)
	if err != nil {
		return "", v1.Hash{}, err
	}
	layers, err := img.Layers()
	if err != nil {
		return "", v1.Hash{}, err
	}
	if baseLayers > len(layers) {
		baseLayers = len(layers)
	}
	for _, l := range layers[:baseLayers] {
		if err := r.ensureBaseLayer(ctx, target, l); err != nil {
			return "", v1.Hash{}, err
		}
	}
	tag := target.Tag(Latest)
	if err := remote.Write(tag, img, r.opts(ctx)...); err != nil {
		return "", v1.Hash{}, fmt.Errorf("push %s: %w", tag, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", v1.Hash{}, err
	}
	return r.Ref(repo, digest), digest, nil
}

// ensureBaseLayer guarantees that target holds layer l: already there, mounted
// from the shared base repository, or (registries without mount support)
// uploaded.
func (r *Registry) ensureBaseLayer(ctx context.Context, target name.Repository, l v1.Layer) error {
	d, err := l.Digest()
	if err != nil {
		return err
	}
	if r.hasBlob(ctx, target, d) {
		return nil
	}
	baseRepo, err := r.repo(BaseRepo)
	if err != nil {
		return err
	}
	if !r.hasBlob(ctx, baseRepo, d) {
		if err := remote.WriteLayer(baseRepo, l, r.opts(ctx)...); err != nil {
			return fmt.Errorf("upload base layer %s: %w", d, err)
		}
	}
	if ok, err := r.mountBlob(ctx, target, baseRepo, d); err == nil && ok {
		return nil
	}
	if err := remote.WriteLayer(target, l, r.opts(ctx)...); err != nil {
		return fmt.Errorf("upload layer %s: %w", d, err)
	}
	return nil
}

func (r *Registry) hasBlob(ctx context.Context, repo name.Repository, d v1.Hash) bool {
	resp, err := r.blobRequest(ctx, http.MethodHead, repo, nil, fmt.Sprintf("blobs/%s", d), nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// mountBlob asks the registry to link a blob of from into target without
// transferring it. It reports false when the registry declined and opened an
// upload session instead.
func (r *Registry) mountBlob(ctx context.Context, target, from name.Repository, d v1.Hash) (bool, error) {
	q := url.Values{"mount": {d.String()}, "from": {from.RepositoryStr()}}
	resp, err := r.blobRequest(ctx, http.MethodPost, target, &from, "blobs/uploads/", q)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusCreated, nil
}

// blobRequest sends one request to /v2/<repo>/<path>, authenticated with pull
// scope on repo (push scope for POST) and pull scope on the optional other repo.
func (r *Registry) blobRequest(ctx context.Context, method string, repo name.Repository, other *name.Repository, path string, q url.Values) (*http.Response, error) {
	scope := repo.Scope(transport.PullScope)
	if method == http.MethodPost {
		scope = repo.Scope(transport.PushScope)
	}
	scopes := []string{scope}
	if other != nil {
		scopes = append(scopes, other.Scope(transport.PullScope))
	}
	auth := r.Auth
	if auth == nil {
		auth = authn.Anonymous
	}
	rt := r.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	tr, err := transport.NewWithContext(ctx, repo.Registry, auth, rt, scopes)
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   repo.Registry.Scheme(),
		Host:     repo.Registry.RegistryStr(),
		Path:     fmt.Sprintf("/v2/%s/%s", repo.RepositoryStr(), path),
		RawQuery: q.Encode(),
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return (&http.Client{Transport: tr}).Do(req)
}

// Repos lists the repositories of the registry.
func (r *Registry) Repos(ctx context.Context) ([]string, error) {
	reg, err := name.NewRegistry(r.Host, name.Insecure)
	if err != nil {
		return nil, err
	}
	repos, err := remote.Catalog(ctx, reg, r.opts(ctx)[1:]...)
	if err != nil {
		return nil, err
	}
	sort.Strings(repos)
	return repos, nil
}

// DeleteRepo deletes every tagged manifest of a repository; the registry's
// garbage collection then frees the blobs and the untagged manifests.
func (r *Registry) DeleteRepo(ctx context.Context, repo string) error {
	target, err := r.repo(repo)
	if err != nil {
		return err
	}
	tags, err := remote.List(target, r.opts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	for _, tag := range tags {
		desc, err := remote.Head(target.Tag(tag), r.opts(ctx)...)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return err
		}
		if err := remote.Delete(target.Digest(desc.Digest.String()), r.opts(ctx)...); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete %s@%s: %w", repo, desc.Digest, err)
		}
	}
	return nil
}

func isNotFound(err error) bool {
	if terr, ok := err.(*transport.Error); ok {
		return terr.StatusCode == http.StatusNotFound
	}
	return strings.Contains(err.Error(), "NAME_UNKNOWN") || strings.Contains(err.Error(), "MANIFEST_UNKNOWN")
}

// Image reads the manifest of repo:latest back (for tests and diagnostics).
func (r *Registry) Image(ctx context.Context, repo string) (v1.Image, error) {
	target, err := r.repo(repo)
	if err != nil {
		return nil, err
	}
	return remote.Image(target.Tag(Latest), r.opts(ctx)...)
}
