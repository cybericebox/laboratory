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
	tlsCfg, err := gateTLS(cfg)
	if err != nil {
		return grpcserver.PrewarmConfig{}, err
	}
	return grpcserver.PrewarmConfig{
		Enabled:      true,
		RegistryAddr: cfg.Cache.GateAddr,
		CacheTLS:     tlsCfg,
		Token:        imagecache.NewTokenFile(cfg.RegistryTokenFile),
		Registries:   cfg.Cache.Registries,
		Resolver: &imagecache.RegistryResolver{
			// No keychain: the digest of a tag is asked anonymously, and only for an image the gate has already served for the call's own
			// credentials; a private image is warmed by digest, with the credentials that arrive with the call. The agent reads no Secret for it.
			TTL:       cfg.Cache.PinTTL,
			Platforms: grpcserver.NodePlatforms(k8s, selector),
		},
		Concurrency: cfg.Cache.Concurrency,
		Timeout:     cfg.Cache.Timeout,
	}, nil
}

// gateTLS is the TLS configuration of the agent towards the registry gate: it trusts the certificate authority in AGENT_PREWARM_GATE_CA_FILE
// (the chart mounts the ConfigMap laboratory-registry-ca there) and nothing else; nil when no file is configured (a development setup).
func gateTLS(cfg *config.Config) (*tls.Config, error) {
	if cfg.Cache.GateCAFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(cfg.Cache.GateCAFile)
	if err != nil {
		return nil, fmt.Errorf("AGENT_PREWARM_GATE_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("AGENT_PREWARM_GATE_CA_FILE: no certificate in %s", cfg.Cache.GateCAFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
