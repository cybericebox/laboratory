package config

import (
	"os"
	"testing"
)

func TestLoadParsesAllowedCNs(t *testing.T) {
	os.Setenv("AGENT_ALLOWED_CLIENT_CNS", "platform,daemon")
	os.Setenv("AGENT_MTLS_ENABLED", "true")
	defer os.Unsetenv("AGENT_ALLOWED_CLIENT_CNS")
	defer os.Unsetenv("AGENT_MTLS_ENABLED")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.MTLS.AllowedClientCNs) != 2 || c.MTLS.AllowedClientCNs[0] != "platform" {
		t.Errorf("AllowedClientCNs = %v", c.MTLS.AllowedClientCNs)
	}
	if !c.MTLS.Enabled {
		t.Error("MTLS.Enabled should be true")
	}
}
