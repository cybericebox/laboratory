package grpc

import (
	"context"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
)

// checkerTTL is how long the answer "the checker DaemonSet exists" is kept.
const checkerTTL = 30 * time.Second

type checkerPresence struct {
	mu      sync.Mutex
	at      time.Time
	present bool
}

// checksAvailable is what features.checks.enabled reports and what a lab with checks is admitted by: the checks option is on AND the
// checker component is there (its DaemonSet exists in the release namespace; a cluster with no lab node yet still has it). An answer
// of the API server other than "not found" keeps the last known one, so a hiccup of the API never turns the feature off. With
// Features.CheckerRequired off (tests, development) only the option counts.
func (h *Handler) checksAvailable() bool {
	if !h.features.ChecksEnabled {
		return false
	}
	if !h.features.CheckerRequired || h.k8s == nil {
		return true
	}
	h.checker.mu.Lock()
	defer h.checker.mu.Unlock()
	if !h.checker.at.IsZero() && time.Since(h.checker.at) < checkerTTL {
		return h.checker.present
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := h.k8s.AppsV1().DaemonSets(names.SystemNamespace).Get(ctx, names.CheckerDaemonSet, metav1.GetOptions{})
	switch {
	case err == nil:
		h.checker.present = true
	case apierrors.IsNotFound(err):
		h.checker.present = false
	case h.checker.at.IsZero():
		h.checker.present = true // never answered yet: the option says it is there
		return h.checker.present
	default:
		return h.checker.present
	}
	h.checker.at = time.Now()
	return h.checker.present
}

// checksUnavailableText says why checks cannot be used, for a refusal.
func (h *Handler) checksUnavailableText() string {
	if !h.features.ChecksEnabled {
		return "the checks option is off on this laboratory (features.checks.enabled is false)"
	}
	return "the checker is not installed on this laboratory (the DaemonSet " + names.CheckerDaemonSet + " does not exist), so checks would never run (features.checks.enabled is false)"
}
