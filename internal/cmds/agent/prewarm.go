package agent

import (
	"encoding/json"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/cybericebox/laboratory/internal/agent/config"
	grpcserver "github.com/cybericebox/laboratory/internal/agent/grpc"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/names"
)

// prewarmConfig builds the cache prewarm setup of the agent from its configuration.
func prewarmConfig(cfg *config.Config, k8s kubernetes.Interface) (grpcserver.PrewarmConfig, error) {
	var selector map[string]string
	if err := json.Unmarshal([]byte(cfg.LabNodeSelector), &selector); err != nil {
		return grpcserver.PrewarmConfig{}, fmt.Errorf("LAB_NODE_SELECTOR: %w", err)
	}
	return grpcserver.PrewarmConfig{
		Enabled:      true,
		RegistryAddr: cfg.RegistryAddr,
		Registries:   cfg.Cache.Registries,
		Resolver: &imagecache.RegistryResolver{
			Keychain:  &grpcserver.SecretKeychain{K8s: k8s, Namespace: names.SystemNamespace, Names: cfg.Cache.PullSecrets},
			TTL:       cfg.Cache.PinTTL,
			Platforms: grpcserver.NodePlatforms(k8s, selector),
		},
		Concurrency: cfg.Cache.Concurrency,
		Timeout:     cfg.Cache.Timeout,
	}, nil
}
