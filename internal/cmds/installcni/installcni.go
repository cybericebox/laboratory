//go:build linux

package installcni

import (
	"log"
	"os"
	"time"

	"github.com/cybericebox/laboratory/internal/nodeagent"
)

func Run() {
	confDir := envOrDefault("CNI_CONF_DIR", nodeagent.CNIConfDir)
	agentSocket := envOrDefault("GRPC_SOCK", "/run/cybericebox/node-agent.sock")
	// Empty (the default) = no fallback: wait for the real CNI config for as long as it takes.
	var fallback time.Duration
	if fallbackStr := os.Getenv("CNI_FALLBACK_TIMEOUT"); fallbackStr != "" {
		var err error
		if fallback, err = time.ParseDuration(fallbackStr); err != nil {
			log.Fatalf("invalid CNI_FALLBACK_TIMEOUT %q: %v", fallbackStr, err)
		}
	}

	if fallback > 0 {
		log.Printf("install-cni: writing %s/%s (explicit opt-in: fallback config after %s without a base CNI)", confDir, nodeagent.CNIConfFile, fallback)
	} else {
		log.Printf("install-cni: writing %s/%s once the base CNI config appears (no fallback)", confDir, nodeagent.CNIConfFile)
	}
	if err := nodeagent.InstallCNIConf(confDir, agentSocket, fallback); err != nil {
		log.Fatalf("install-cni failed: %v", err)
	}
	log.Printf("install-cni: done")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
