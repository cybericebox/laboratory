package laboratory

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// UpstreamSweeper removes the credentials Secrets of labs that no longer exist from laboratory-upstream. The Lab controller deletes the
// Secret of a lab when it sees the lab gone, but a missed event (a restart, a lost watch) would leave it for good, and a Secret of another
// namespace cannot be owned by the Lab. The agent writes the namespace and name of the lab in annotations; a Secret without them is not ours
// to judge and stays. A Secret younger than MinAge stays too: the agent writes it right after the Lab, and the check must not race that.
type UpstreamSweeper struct {
	Client client.Client
	// Reader reads straight from the API server; nil means Client.
	Reader client.Reader
	// Interval is the time between sweeps (10 minutes when zero), MinAge how old a Secret must be to be judged (10 minutes when zero).
	Interval, MinAge time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// NeedLeaderElection makes the sweeper run on the elected operator only.
func (s *UpstreamSweeper) NeedLeaderElection() bool { return true }

// Start runs the sweep until ctx ends.
func (s *UpstreamSweeper) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("upstream-sweeper")
	interval := s.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.Sweep(ctx); err != nil {
			log.Error(err, "upstream sweep")
		} else if n > 0 {
			log.Info("removed the credentials of labs that are gone", "secrets", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Sweep removes the orphaned Secrets and returns how many.
func (s *UpstreamSweeper) Sweep(ctx context.Context) (int, error) {
	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	minAge := s.MinAge
	if minAge <= 0 {
		minAge = 10 * time.Minute
	}
	var list corev1.SecretList
	if err := reader.List(ctx, &list, client.InNamespace(names.UpstreamNamespace)); err != nil {
		return 0, err
	}
	removed := 0
	for i := range list.Items {
		sec := &list.Items[i]
		if !strings.HasPrefix(sec.Name, "lab-") || now().Sub(sec.CreationTimestamp.Time) < minAge {
			continue
		}
		ns, lab := sec.Annotations[names.AnnotationUpstreamLabNamespace], sec.Annotations[names.AnnotationUpstreamLab]
		if ns == "" || lab == "" || names.UpstreamLabSecret(ns, lab) != sec.Name {
			continue // not written by the agent for a lab we can name
		}
		var l laboratoryv1alpha1.Lab
		err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: lab}, &l)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return removed, err
		}
		if err := client.IgnoreNotFound(s.Client.Delete(ctx, sec)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
