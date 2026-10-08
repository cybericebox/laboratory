package laboratory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// RetentionConfigMap holds, per lab whose snapshots outlive it, the time the
// sweep first saw the lab gone. It lives in the operator namespace.
const RetentionConfigMap = "laboratory-snapshot-retention"

// retentionDone marks a lab whose repositories were deleted.
const retentionDone = "deleted"

// SnapshotCatalog is the registry side of the retention sweep.
type SnapshotCatalog interface {
	Repos(ctx context.Context) ([]string, error)
	DeleteRepo(ctx context.Context, repo string) error
}

// RetentionSweeper deletes the snapshots of labs that are gone once Retention
// has passed since the sweep first noticed the lab missing. The registry's
// garbage collection then frees the blobs (see the chart values
// statePersistence.registry.gc). A lab that reappears under the same name
// before the deadline cancels the deletion.
type RetentionSweeper struct {
	Client client.Client
	// Reader reads Labs and the ConfigMap directly from the API server: the
	// operator's cache does not watch ConfigMaps cluster-wide. Nil means Client.
	Reader    client.Reader
	Registry  SnapshotCatalog
	Retention time.Duration
	Interval  time.Duration
	Namespace string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// NeedLeaderElection makes the sweep run on the elected operator only.
func (s *RetentionSweeper) NeedLeaderElection() bool { return true }

// Start runs the sweep until ctx ends.
func (s *RetentionSweeper) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("snapshot-retention")
	interval := s.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sweep(ctx); err != nil {
			log.Error(err, "retention sweep")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// labKeyOf is the ConfigMap key of the lab a repository belongs to:
// "<namespace>_<lab>" (a ConfigMap key cannot hold a slash; Kubernetes names have no underscore).
func labKeyOf(repo string) (key string, ok bool) {
	parts := strings.Split(repo, "/")
	if len(parts) != 4 || parts[0] != snapshot.RepoPrefix {
		return "", false
	}
	return parts[1] + "_" + parts[2], true
}

// Sweep runs one retention pass.
func (s *RetentionSweeper) Sweep(ctx context.Context) error {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	repos, err := s.Registry.Repos(ctx)
	if err != nil {
		return fmt.Errorf("list snapshot repositories: %w", err)
	}
	groups := map[string][]string{}
	for _, repo := range repos {
		if key, ok := labKeyOf(repo); ok {
			groups[key] = append(groups[key], repo)
		}
	}

	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	key := types.NamespacedName{Namespace: s.Namespace, Name: RetentionConfigMap}
	var cm corev1.ConfigMap
	create := false
	if err := reader.Get(ctx, key, &cm); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		create = true
	}
	orig := cm.DeepCopy()
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	var sweepErr error
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ns, lab, _ := strings.Cut(k, "_")
		var l laboratoryv1alpha1.Lab
		err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: lab}, &l)
		if err == nil {
			delete(cm.Data, k) // legacy missing-object tombstone never overrides a live identity
			if err := s.retireRetained(ctx, reader, &l, groups[k], now); err != nil {
				sweepErr = err
			}
			continue
		}
		if !errors.IsNotFound(err) {
			sweepErr = err
			continue
		}
		since, ok := time.Time{}, false
		if raw, have := cm.Data[k]; have {
			if raw == retentionDone {
				continue // deleted; the registry still lists the empty repository until its GC
			}
			since, err = time.Parse(time.RFC3339, raw)
			ok = err == nil
		}
		if !ok {
			cm.Data[k] = now.UTC().Format(time.RFC3339)
			continue
		}
		if now.Sub(since) < s.Retention {
			continue
		}
		deleted := true
		for _, repo := range groups[k] {
			if err := s.Registry.DeleteRepo(ctx, repo); err != nil {
				sweepErr, deleted = err, false
			}
		}
		if deleted {
			cm.Data[k] = retentionDone
		}
	}
	// Forget labs whose repositories are gone.
	for k := range cm.Data {
		if _, ok := groups[k]; !ok {
			delete(cm.Data, k)
		}
	}

	switch {
	case create && len(cm.Data) > 0:
		if err := s.Client.Create(ctx, &cm); err != nil {
			return err
		}
	case !create:
		if err := s.Client.Patch(ctx, &cm, client.MergeFrom(orig)); err != nil {
			return err
		}
	}
	return sweepErr
}
