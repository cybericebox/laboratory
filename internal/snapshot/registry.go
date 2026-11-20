package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

// Registry is a client of the snapshot registry: the registry gate, as a node reaches it through the relay of the node-agent, or as the
// operator reaches it with its own token. It has no credentials of its own: the identity is the transport's (the relay gives the node's
// token, the operator's transport the operator's).
type Registry struct {
	// Host is host:port of the registry as it appears in every name and reference, e.g. "localhost:5035". Hosts on the loopback or a
	// private network are spoken to over plain HTTP.
	Host string
	// Transport is the HTTP transport: on a node it dials the unix socket of the relay's push listener (devicestate.UnixTransport); for
	// the operator it adds the operator's token; tests use their own. Nil: http.DefaultTransport.
	Transport http.RoundTripper
}

// The stable codes of the pushes the registry gate refuses by a rule of its own (the OCI error "code"); the gate defines them.
const (
	CodePushInterval = "CIB_PUSH_INTERVAL"
	CodePushBudget   = "CIB_PUSH_BUDGET"
	CodeExitLimit    = "CIB_EXIT_LIMIT"
	CodeTenantQuota  = "CIB_TENANT_QUOTA"
	CodeRegistryFull = "CIB_REGISTRY_FULL"
)

// Refusal is a push the gate refused by a rule of its own: 429 with Retry-After (the interval, the budget, the exit limit), 403
// CIB_TENANT_QUOTA, 507 CIB_REGISTRY_FULL.
type Refusal struct {
	Status int
	// Code is the CIB_* code, "" when the answer had none.
	Code string
	// RetryAfter is the Retry-After the gate sent (0: none).
	RetryAfter time.Duration
	Message    string
}

func (r *Refusal) Error() string {
	msg := r.Message
	if msg == "" {
		msg = http.StatusText(r.Status)
	}
	if r.Code != "" {
		return fmt.Sprintf("the registry refused the push (%d %s): %s", r.Status, r.Code, msg)
	}
	return fmt.Sprintf("the registry refused the push (%d): %s", r.Status, msg)
}

type noteKey struct{}

// note carries the Retry-After of the last refused request of one operation: the error of the client library does not have the headers.
type note struct {
	mu    sync.Mutex
	retry time.Duration
}

// observe records the Retry-After of every 429 under the operation's note.
type observe struct{ next http.RoundTripper }

func (o observe) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.next.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		if n, ok := req.Context().Value(noteKey{}).(*note); ok {
			if secs, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && secs > 0 {
				n.mu.Lock()
				n.retry = time.Duration(secs) * time.Second
				n.mu.Unlock()
			}
		}
	}
	return resp, err
}

func (r *Registry) opts(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithTransport(r.roundTripper()), remote.WithRetryPredicate(retryable)}
}

// retryable says whether the client library asks again by itself: a transient failure of the network or the server, but never a refusal
// by a rule of the gate (429 or a CIB_* code), whose answer says when to come back; asking again at once would only count against the limit.
func retryable(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		if terr.StatusCode == http.StatusTooManyRequests || terr.StatusCode == http.StatusInsufficientStorage {
			return false
		}
		for _, d := range terr.Errors {
			if strings.HasPrefix(string(d.Code), "CIB_") {
				return false
			}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

func (r *Registry) roundTripper() http.RoundTripper {
	if r.Transport != nil {
		return observe{r.Transport}
	}
	return observe{http.DefaultTransport}
}

// refusal turns the error of a push into a *Refusal when the gate refused it by a rule of its own (429, or a CIB_* code); any other error
// is returned as it is.
func refusal(err error, n *note) error {
	var terr *transport.Error
	if err == nil || !errors.As(err, &terr) {
		return err
	}
	code, msg := "", ""
	for _, d := range terr.Errors {
		if strings.HasPrefix(string(d.Code), "CIB_") {
			code, msg = string(d.Code), d.Message
			break
		}
	}
	if terr.StatusCode != http.StatusTooManyRequests && code == "" && terr.StatusCode != http.StatusInsufficientStorage {
		return err
	}
	if msg == "" && len(terr.Errors) > 0 {
		msg = terr.Errors[0].Message
	}
	n.mu.Lock()
	retry := n.retry
	n.mu.Unlock()
	return &Refusal{Status: terr.StatusCode, Code: code, RetryAfter: retry, Message: msg}
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
// sourceRepo, when set, is a repository that already holds the base layers (the
// image cache's repository of the base image): they are mounted from it, so
// nothing is uploaded at all. It returns the pullable reference by digest.
func (r *Registry) Push(ctx context.Context, repo string, img v1.Image, baseLayers int, sourceRepo string) (string, v1.Hash, error) {
	n := &note{}
	ctx = context.WithValue(ctx, noteKey{}, n)
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
		if err := r.ensureBaseLayer(ctx, target, l, sourceRepo); err != nil {
			return "", v1.Hash{}, refusal(err, n)
		}
	}
	tag := target.Tag(Latest)
	if err := remote.Write(tag, img, r.opts(ctx)...); err != nil {
		return "", v1.Hash{}, refusal(fmt.Errorf("push %s: %w", tag, err), n)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", v1.Hash{}, err
	}
	return r.Ref(repo, digest), digest, nil
}

// ensureBaseLayer guarantees that target holds layer l: already there, mounted from sourceRepo (the cache repository of the base image,
// which the gate allows as the source of a mount), or uploaded. The node has no access to a shared repository of its own: it
// reads the cache and writes its own device repository.
func (r *Registry) ensureBaseLayer(ctx context.Context, target name.Repository, l v1.Layer, sourceRepo string) error {
	d, err := l.Digest()
	if err != nil {
		return err
	}
	if r.HasBlob(ctx, target, d) {
		return nil
	}
	if sourceRepo != "" {
		if src, err := r.repo(sourceRepo); err == nil {
			if ok, err := r.mountBlob(ctx, target, src, d); err == nil && ok {
				return nil
			}
		}
	}
	if err := remote.WriteLayer(target, l, r.opts(ctx)...); err != nil {
		return fmt.Errorf("upload layer %s: %w", d, err)
	}
	return nil
}

func (r *Registry) HasBlob(ctx context.Context, repo name.Repository, d v1.Hash) bool {
	resp, err := r.blobRequest(ctx, http.MethodHead, repo, nil, fmt.Sprintf("blobs/%s", d), nil)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
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
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusCreated, nil
}

// blobRequest sends one request to /v2/<repo>/<path>, authenticated with pull
// scope on repo (push scope for POST) and pull scope on the optional other repo.
func (r *Registry) blobRequest(ctx context.Context, method string, repo name.Repository, other *name.Repository, path string, q url.Values) (*http.Response, error) {
	scope := repo.Scope(transport.PullScope)
	if method == http.MethodPost || method == http.MethodDelete {
		scope = repo.Scope(transport.PushScope)
	}
	scopes := []string{scope}
	if other != nil {
		scopes = append(scopes, other.Scope(transport.PullScope))
	}
	tr, err := transport.NewWithContext(ctx, repo.Registry, authn.Anonymous, r.roundTripper(), scopes)
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   repo.Scheme(),
		Host:     repo.RegistryStr(),
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

// HasBlobIn reports whether the blob is stored in the repository named by its path.
func (r *Registry) HasBlobIn(ctx context.Context, repo string, d v1.Hash) bool {
	target, err := r.repo(repo)
	if err != nil {
		return false
	}
	return r.HasBlob(ctx, target, d)
}

// Repo and Options expose the repository name and request options to tests of other packages.
func (r *Registry) Repo(repo string) (name.Repository, error)   { return r.repo(repo) }
func (r *Registry) Options(ctx context.Context) []remote.Option { return r.opts(ctx) }
