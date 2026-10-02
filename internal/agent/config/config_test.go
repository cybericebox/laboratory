package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaultsAndTenantSettings(t *testing.T) {
	os.Setenv("AGENT_MTLS_ENABLED", "true")
	os.Setenv("AGENT_LAB_TOLERATIONS", `[{"key":"lab","operator":"Exists"}]`)
	defer os.Unsetenv("AGENT_MTLS_ENABLED")
	defer os.Unsetenv("AGENT_LAB_TOLERATIONS")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MTLS.Enabled {
		t.Error("MTLS.Enabled should be true")
	}
	// The code defaults mirror the chart values.
	if c.Cache.PinTTL != 30*time.Minute {
		t.Errorf("AGENT_CACHE_PIN_TTL default %v, the chart says 30m", c.Cache.PinTTL)
	}
	if c.LabNodeSelector != "{}" || c.LabTolerations == "[]" || c.TenantStatusInterval != 30*time.Second {
		t.Errorf("%+v", c)
	}
}

func TestFeatureDefaultsMirrorTheChart(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.State.Debounce != 5*time.Second || c.State.WriteQuota != "512Mi" || c.State.MaxFileSize != "256Mi" ||
		len(c.State.ExcludePaths) != 3 || !c.Scheduler.Enabled || c.Scheduler.MaxPods != 20 {
		t.Fatalf("%+v %+v", c.State, c.Scheduler)
	}
}
