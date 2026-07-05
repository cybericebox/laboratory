//go:build linux

package main

import (
	"log"
	"os"
	"time"
	
	"github.com/cybericebox/laboratory/internal/nodeagent"
)

func main() {
	confDir := envOrDefault("CNI_CONF_DIR", nodeagent.CNIConfDir)
	agentSocket := envOrDefault("GRPC_SOCK", "/run/cybericebox/node-agent.sock")
	fallbackStr := envOrDefault("CNI_FALLBACK_TIMEOUT", "60s")
	
	fallback, err := time.ParseDuration(fallbackStr)
	if err != nil {
		log.Fatalf("invalid CNI_FALLBACK_TIMEOUT %q: %v", fallbackStr, err)
	}
	
	log.Printf("install-cni: writing %s/%s (fallback after %s)", confDir, nodeagent.CNIConfFile, fallback)
	if err = nodeagent.InstallCNIConf(confDir, agentSocket, fallback); err != nil {
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
