package snapshot

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DeleteSnapshot removes only the immutable manifest referenced by an owned
// Device. No cache/base/shared blob or physical GC is touched.
func (r *Registry) DeleteSnapshot(ctx context.Context, repo, image string) error {
	if image == "" {
		return nil
	}
	expected, err := r.repo(repo)
	if err != nil {
		return err
	}
	digest, err := name.NewDigest(image, name.Insecure)
	if err != nil {
		return err
	}
	if digest.Repository.Name() != expected.Name() {
		return fmt.Errorf("snapshot manifest is outside the exact owned repository")
	}
	if err = remote.Delete(digest, r.opts(ctx)...); err != nil && !isNotFound(err) {
		return err
	}
	if _, err = remote.Head(digest, r.opts(ctx)...); err == nil {
		return fmt.Errorf("snapshot manifest remains present")
	} else if !isNotFound(err) {
		return err
	}
	return nil
}

// RepositoryEmpty confirms actual absence of tagged manifests after DeleteRepo.
// Untagged history/shared blobs remain physical unknown until verified GC.
func (r *Registry) RepositoryEmpty(ctx context.Context, repo string) (bool, error) {
	target, err := r.repo(repo)
	if err != nil {
		return false, err
	}
	tags, err := remote.List(target, r.opts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return len(tags) == 0, nil
}
