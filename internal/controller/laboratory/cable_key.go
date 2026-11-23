package laboratory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CableKeySecret is the Secret of the device access namespace with the master key of the cable stream between the device access pods
// (console cables): "key", and during a rotation "previous".
const CableKeySecret = "laboratory-console-cable-key"

// CableKeyEnsurer makes the cable key Secret when it is missing, with 32 random bytes, and does nothing else: it never reads it, never
// changes it and never deletes it (its only right in that namespace is create). A key made by the chart at render time would be a new one
// at every helm template or GitOps render; made here, it is made once. To rotate, put the old key under "previous" and a new one under
// "key" by hand, then remove "previous" (DEPLOY.md, "Console cables"); deleting the Secret makes the operator create a fresh key.
type CableKeyEnsurer struct {
	Client    client.Client
	Namespace string
	// Interval is how often a missing Secret is made again; zero means five minutes.
	Interval time.Duration
}

// NeedLeaderElection: one operator makes it.
func (e *CableKeyEnsurer) NeedLeaderElection() bool { return true }

// Start makes the Secret now and then at every interval, until ctx ends.
func (e *CableKeyEnsurer) Start(ctx context.Context) error {
	interval := e.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := e.Ensure(ctx); err != nil && ctx.Err() == nil {
			ctrl.Log.WithName("cable-key").Error(err, "make the cable key Secret; retrying")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Ensure creates the Secret; one that exists already is left as it is.
func (e *CableKeyEnsurer) Ensure(ctx context.Context) error {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CableKeySecret, Namespace: e.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "laboratory-operator"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"key": []byte(hex.EncodeToString(raw[:]))},
	}
	if err := e.Client.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s/%s: %w", e.Namespace, CableKeySecret, err)
	}
	return nil
}
