package agent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"

	"github.com/cybericebox/laboratory/internal/agent/config"
	grpcserver "github.com/cybericebox/laboratory/internal/agent/grpc"
	"github.com/cybericebox/laboratory/internal/imagecache"
)

// prewarmConfig builds the cache prewarm setup of the agent from its configuration.
func prewarmConfig(cfg *config.Config, k8s kubernetes.Interface) (grpcserver.PrewarmConfig, error) {
	var selector map[string]string
	if err := json.Unmarshal([]byte(cfg.LabNodeSelector), &selector); err != nil {
		return grpcserver.PrewarmConfig{}, fmt.Errorf("LAB_NODE_SELECTOR: %w", err)
	}
	var tlsCfg *tls.Config
	if cfg.Cache.GateCAFile != "" {
		pem, err := os.ReadFile(cfg.Cache.GateCAFile)
		if err != nil {
			return grpcserver.PrewarmConfig{}, fmt.Errorf("AGENT_PREWARM_GATE_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return grpcserver.PrewarmConfig{}, fmt.Errorf("AGENT_PREWARM_GATE_CA_FILE: no certificate in %s", cfg.Cache.GateCAFile)
		}
		tlsCfg = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return grpcserver.PrewarmConfig{
		Enabled:      true,
		RegistryAddr: cfg.Cache.GateAddr,
		CacheTLS:     tlsCfg,
		Registries:   cfg.Cache.Registries,
		Resolver: &imagecache.RegistryResolver{
			Keychain:  &grpcserver.SecretKeychain{K8s: k8s, Namespace: cfg.Cache.PullSecretNamespace, Names: cfg.Cache.PullSecrets},
			TTL:       cfg.Cache.PinTTL,
			Platforms: grpcserver.NodePlatforms(k8s, selector),
		},
		Concurrency: cfg.Cache.Concurrency,
		Timeout:     cfg.Cache.Timeout,
	}, nil
}
