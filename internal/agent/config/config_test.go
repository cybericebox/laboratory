package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaultsAndTenantSettings(t *testing.T) {
	os.Setenv("LAB_TOLERATIONS", `[{"key":"lab","operator":"Exists"}]`)
	defer os.Unsetenv("LAB_TOLERATIONS")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MTLSEnabled() {
		t.Error("mTLS is on unless AGENT_ALLOW_INSECURE")
	}
	if c.ServerTLS.CertFile != "/tls/tls.crt" || c.MTLS.ClientCAKeyFile != "/ca/tls.key" {
		t.Errorf("the chart's mount paths are baked in: %+v %+v", c.ServerTLS, c.MTLS)
	}
	if c.RegistryAddr != "laboratory-registry.laboratory-system.svc:5000" || c.Cache.NodePrefix != "localhost:5035" {
		t.Errorf("the registry addresses are derived: %q %q", c.RegistryAddr, c.Cache.NodePrefix)
	}
	// The code defaults mirror the chart values.
	if c.Cache.PinTTL != 30*time.Minute {
		t.Errorf("IMAGE_CACHE_PIN_TTL default %v, the chart says 30m", c.Cache.PinTTL)
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
	if c.State.Debounce != 5*time.Second || c.State.TenantQuota != "10Gi" || c.State.MaxEntries != 100000 || c.State.WriteQuota != "512Mi" || c.State.MaxFileSize != "256Mi" ||
		len(c.State.ExcludePaths) != 3 || !c.Scheduler.Enabled || c.Scheduler.MaxPods != 20 ||
		c.Limits.DeviceMaxCPU != "2000m" || c.Limits.LabMaxDevices != 32 || c.Limits.TenantMaxLabs != 0 ||
		c.ProxyAccessTokenMaxTTL != 60*time.Second || c.ProxySessionIdleTTL != 24*time.Hour || c.ProxySessionMaxTTL != 168*time.Hour || c.Limits.GroupMaxLabs != 50 {
		t.Fatalf("%+v %+v", c.State, c.Scheduler)
	}
}
