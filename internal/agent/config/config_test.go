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

// The monitoring sizes and the namespaces are optional environment variables with fixed defaults.
func TestLoadTuningKnobs(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m := c.Monitoring
	if m.JournalSize != 10000 || m.JournalAge != 15*time.Minute || m.PollInterval != time.Second || m.SubscriberBuffer != 256 || m.MaxStreamsPerTenant != 8 {
		t.Errorf("monitoring defaults: %+v", m)
	}
	if c.ReleaseNamespace != "laboratory-system" || c.Cache.PullSecretNamespace != "laboratory-system" {
		t.Errorf("namespaces: %q %q", c.ReleaseNamespace, c.Cache.PullSecretNamespace)
	}
	if c.PublicVPNEndpoint != "vpn.example.com:51820" {
		t.Errorf("a host without a port gets the default port: %q", c.PublicVPNEndpoint)
	}
	t.Setenv("AGENT_MONITORING_JOURNAL_SIZE", "5")
	t.Setenv("AGENT_MONITORING_JOURNAL_AGE", "1m")
	t.Setenv("AGENT_MONITORING_POLL_INTERVAL", "3s")
	t.Setenv("AGENT_MONITORING_SUBSCRIBER_BUFFER", "7")
	t.Setenv("AGENT_MONITORING_MAX_STREAMS_PER_TENANT", "2")
	t.Setenv("AGENT_RELEASE_NAMESPACE", "rel")
	t.Setenv("AGENT_PULL_SECRET_NAMESPACE", "pull")
	t.Setenv("AGENT_REGISTRY_ADDR", "reg.example:5000")
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:443")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	m = c.Monitoring
	if m.JournalSize != 5 || m.JournalAge != time.Minute || m.PollInterval != 3*time.Second || m.SubscriberBuffer != 7 || m.MaxStreamsPerTenant != 2 {
		t.Errorf("monitoring overrides: %+v", m)
	}
	if c.ReleaseNamespace != "rel" || c.Cache.PullSecretNamespace != "pull" || c.RegistryAddr != "reg.example:5000" || c.PublicVPNEndpoint != "vpn.example.com:443" {
		t.Errorf("overrides: %q %q %q %q", c.ReleaseNamespace, c.Cache.PullSecretNamespace, c.RegistryAddr, c.PublicVPNEndpoint)
	}
}
