package grpc

import (
	"context"
	"sort"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// SecretKeychain authenticates requests to upstream registries with
// dockerconfigjson Secrets of one namespace, read on every use (a rotated
// credential applies at once). A missing Secret means anonymous access.
type SecretKeychain struct {
	K8s       kubernetes.Interface
	Namespace string
	Names     []string
}

// Resolve implements authn.Keychain.
func (k *SecretKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	var docs [][]byte
	for _, n := range k.Names {
		s, err := k.K8s.CoreV1().Secrets(k.Namespace).Get(context.Background(), n, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if doc := s.Data[corev1.DockerConfigJsonKey]; len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	kc, err := imagecache.NewDockerConfigKeychain(docs...)
	if err != nil {
		return authn.Anonymous, nil
	}
	return kc.Resolve(res)
}

// NodePlatforms lists the distinct OS/architecture pairs of the nodes matching a nodeSelector.
func NodePlatforms(k8s kubernetes.Interface, selector map[string]string) func(context.Context) []v1.Platform {
	return func(ctx context.Context) []v1.Platform {
		nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(selector).String()})
		if err != nil {
			return nil
		}
		seen := map[string]v1.Platform{}
		for _, n := range nodes.Items {
			os, arch := n.Labels[corev1.LabelOSStable], n.Labels[corev1.LabelArchStable]
			if os != "" && arch != "" {
				seen[os+"/"+arch] = v1.Platform{OS: os, Architecture: arch}
			}
		}
		out := make([]v1.Platform, 0, len(seen))
		for _, p := range seen {
			out = append(out, p)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Architecture < out[j].Architecture })
		return out
	}
}
