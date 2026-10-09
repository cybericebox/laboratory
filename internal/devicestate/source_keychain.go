package devicestate

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

// NewPodSourceKeychain reads only the current device pod's own pull Secrets.
// Snapshot-registry write credentials are not part of this source credential path.
func NewPodSourceKeychain(reader client.Reader, nodeName string) func(context.Context, Container, PodInfo) (authn.Keychain, error) {
	return func(ctx context.Context, c Container, p PodInfo) (authn.Keychain, error) {
		if reader == nil || nodeName == "" || p.Device.Namespace == "" || p.Device.Name == "" || p.Pod == "" || p.UID == "" || c.ID == "" || p.ContainerID != c.ID || p.Epoch != p.DeviceEpoch {
			return nil, fmt.Errorf("source image pod ownership is unavailable")
		}
		var pod corev1.Pod
		if err := reader.Get(ctx, types.NamespacedName{Namespace: p.Device.Namespace, Name: p.Pod}, &pod); err != nil {
			return nil, fmt.Errorf("read source image pod: %w", err)
		}
		if pod.Namespace != p.Device.Namespace || pod.Name != p.Pod || string(pod.UID) != p.UID || pod.Spec.NodeName != nodeName || pod.Annotations[names.AnnotationStateDevice] != p.Device.Name {
			return nil, ErrStale
		}
		for key, want := range map[string]int32{names.AnnotationStateEpoch: p.Epoch, names.AnnotationStateIncarnation: p.Incarnation} {
			raw, ok := pod.Annotations[key]
			n, err := strconv.ParseInt(raw, 10, 32)
			if !ok || err != nil || n < 0 || int32(n) != want || strconv.FormatInt(n, 10) != raw {
				return nil, ErrStale
			}
		}
		source, err := name.ParseReference(c.ImageRef)
		if err != nil {
			return nil, fmt.Errorf("invalid source image reference: %w", err)
		}
		var status *corev1.ContainerStatus
		for i := range pod.Status.ContainerStatuses {
			s := &pod.Status.ContainerStatuses[i]
			if containerID(s.ContainerID) == c.ID {
				if status != nil {
					return nil, fmt.Errorf("ambiguous source image container")
				}
				status = s
			}
		}
		if status == nil {
			return nil, ErrStale
		}
		statusRef, err := name.ParseReference(status.Image)
		if err != nil || statusRef.Context().Name() != source.Context().Name() {
			return nil, ErrStale
		}
		matched := false
		for _, container := range pod.Spec.Containers {
			if container.Name != status.Name {
				continue
			}
			ref, err := name.ParseReference(container.Image)
			if err != nil || ref.Context().Name() != source.Context().Name() || matched {
				return nil, ErrStale
			}
			matched = true
		}
		if !matched {
			return nil, ErrStale
		}
		docs := make([][]byte, 0, len(pod.Spec.ImagePullSecrets))
		for _, ref := range pod.Spec.ImagePullSecrets {
			if ref.Name == "" {
				return nil, fmt.Errorf("source image pull Secret name is missing")
			}
			var secret corev1.Secret
			if err := reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &secret); err != nil {
				return nil, fmt.Errorf("read source image pull Secret: %w", err)
			}
			if secret.Namespace != pod.Namespace || secret.Name != ref.Name {
				return nil, ErrStale
			}
			doc := secret.Data[corev1.DockerConfigJsonKey]
			if secret.Type != corev1.SecretTypeDockerConfigJson || len(doc) == 0 {
				return nil, fmt.Errorf("source image pull Secret has no dockerconfigjson")
			}
			var shape struct {
				Auths map[string]json.RawMessage `json:"auths"`
			}
			if err := json.Unmarshal(doc, &shape); err != nil || shape.Auths == nil {
				return nil, fmt.Errorf("source image pull Secret has malformed dockerconfigjson")
			}
			docs = append(docs, doc)
		}
		keychain, err := imagecache.NewDockerConfigKeychain(docs...)
		if err != nil {
			return nil, fmt.Errorf("source image pull Secret credentials: %w", err)
		}
		return sourceRepositoryKeychain{repo: source.Context().Name(), keychain: keychain}, nil
	}
}

type sourceRepositoryKeychain struct {
	repo     string
	keychain authn.Keychain
}

func (k sourceRepositoryKeychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	if resource.String() != k.repo {
		return authn.Anonymous, nil
	}
	return k.keychain.Resolve(resource)
}
