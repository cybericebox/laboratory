package registrygate

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// SnapshotStore is what the gate does to the snapshot repositories (lab/**) itself. *Zot implements it; a node never can do any of it.
type SnapshotStore interface {
	Head(ctx context.Context, repo, kind, ref string) (http.Header, bool, error)
	ManifestBody(ctx context.Context, repo, ref string) ([]byte, error)
	DeleteManifest(ctx context.Context, repo, digest string) error
	DeleteBlob(ctx context.Context, repo, digest string) error
	Repos(ctx context.Context) ([]string, error)
}

// manifestBlobs are the config and layer blobs a manifest lists (with their sizes). An index lists no blobs of its own.
func manifestBlobs(body []byte) (map[string]int64, bool) {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	out := map[string]int64{}
	if m.Config.Digest != "" {
		out[m.Config.Digest] = m.Config.Size
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			out[l.Digest] = l.Size
		}
	}
	return out, true
}

type supersession struct {
	repo, old, keep string
	due             time.Time
}

// Superseder deletes what a newer snapshot has replaced, once the grace has passed (the grace lets a pod that was just created from the
// old snapshot still pull it): the old manifest, and the blobs only it used. It is the logic of snapshot.Registry.Supersede, moved into
// the gate: nodes have no delete. Plans are memory only: after a restart the registry's garbage collection and the operator's retention
// sweep are what is left, and the cost is bytes kept for longer, never data lost.
type Superseder struct {
	Store SnapshotStore
	Grace time.Duration
	Now   func() time.Time
	// OnError is told about a deletion that failed (the plan is dropped; nothing is retried).
	OnError func(repo string, err error)

	mu    sync.Mutex
	tasks []supersession
}

func (s *Superseder) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Plan notes that keep replaced old in repo (both are manifest digests).
func (s *Superseder) Plan(repo, old, keep string) {
	if old == "" || old == keep {
		return
	}
	s.mu.Lock()
	s.tasks = append(s.tasks, supersession{repo: repo, old: old, keep: keep, due: s.now().Add(s.Grace)})
	s.mu.Unlock()
}

// Pending is the number of plans waiting.
func (s *Superseder) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

// Sweep carries out the plans whose grace has passed and returns how many it did.
func (s *Superseder) Sweep(ctx context.Context) int {
	now := s.now()
	s.mu.Lock()
	var due, rest []supersession
	for _, t := range s.tasks {
		if now.Before(t.due) {
			rest = append(rest, t)
		} else {
			due = append(due, t)
		}
	}
	s.tasks = rest
	s.mu.Unlock()
	for _, t := range due {
		if err := s.apply(ctx, t); err != nil && s.OnError != nil {
			s.OnError(t.repo, err)
		}
	}
	return len(due)
}

// isLatest says whether the manifest being superseded is the current "latest" again (the same content pushed again within the grace).
func (s *Superseder) isLatest(ctx context.Context, t supersession) (bool, error) {
	h, ok, err := s.Store.Head(ctx, t.repo, kindManifests, tagLatest)
	if err != nil {
		return false, err
	}
	return ok && h.Get("Docker-Content-Digest") == t.old, nil
}

func (s *Superseder) apply(ctx context.Context, t supersession) error {
	// the current latest is never deleted
	if again, err := s.isLatest(ctx, t); err != nil || again {
		return err
	}
	oldBody, err := s.Store.ManifestBody(ctx, t.repo, t.old)
	if err != nil || oldBody == nil {
		return err // gone already (or unreadable: nothing is deleted that cannot be accounted for)
	}
	oldBlobs, ok := manifestBlobs(oldBody)
	if !ok {
		return nil
	}
	// what must stay: the blobs of the snapshot that replaced it and of whatever is current now
	keep := map[string]bool{}
	for _, ref := range []string{t.keep, tagLatest} {
		body, err := s.Store.ManifestBody(ctx, t.repo, ref)
		if err != nil {
			return err
		}
		if body == nil {
			continue
		}
		if bl, ok := manifestBlobs(body); ok {
			for d := range bl {
				keep[d] = true
			}
		}
	}
	if len(keep) == 0 {
		return nil // the replacement is not there: delete nothing
	}
	// zot has no conditional delete: the check is repeated right before each destructive step, so that a push of the old content that made it
	// "latest" again in the meantime is noticed at the latest possible moment
	if again, err := s.isLatest(ctx, t); err != nil || again {
		return err
	}
	if err := s.Store.DeleteManifest(ctx, t.repo, t.old); err != nil {
		return err
	}
	for d := range oldBlobs {
		if keep[d] {
			continue
		}
		if again, err := s.isLatest(ctx, t); err != nil || again {
			return err
		}
		if err := s.Store.DeleteBlob(ctx, t.repo, d); err != nil {
			return err
		}
	}
	return nil
}

// Run sweeps every interval until ctx ends.
func (s *Superseder) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}
