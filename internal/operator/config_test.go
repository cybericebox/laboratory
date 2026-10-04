package operator

import (
	"testing"
	"time"
)

func TestLoadConfigRequiresLabDomains(t *testing.T) {
	set := func(vpn, base string) {
		t.Setenv("PUBLIC_VPN_ENDPOINT", vpn)
		t.Setenv("BASE_DOMAIN", base)
		setRequiredEnv(t)
	}
	set("vpn.example.com:51820", "labs.example.com")
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"no vpn endpoint": {"", "labs.example.com"},
		"no base domain":  {"vpn.example.com:51820", ""},
	} {
		set(c[0], c[1])
		if _, err := LoadConfig(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestLoadConfigImagePullSecrets(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil || len(cfg.ImagePullSecrets) != 0 {
		t.Fatalf("default: %v %v", cfg, err)
	}
	t.Setenv("IMAGE_PULL_SECRETS", "regcred,other")
	cfg, err = LoadConfig()
	if err != nil || len(cfg.ImagePullSecrets) != 2 || cfg.ImagePullSecrets[1] != "other" {
		t.Fatalf("parsed: %v %v", cfg, err)
	}
}

func TestLoadConfigSchedulerDefaultsAndValidation(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SchedulerEnabled || cfg.SchedulerMaxPods != 20 || cfg.SchedulerStartupTimeout.String() != "5m0s" ||
		cfg.SchedulerRestartThreshold != 5 || cfg.SchedulerPlatformReservePercent != 10 || cfg.SchedulerPlatformReserveCPU != "0" || cfg.SchedulerPlatformReserveMemory != "0" ||
		!cfg.SchedulerResourceCheck || !cfg.SchedulerPrepull || cfg.SchedulerPrepullTimeout.String() != "5m0s" ||
		cfg.DeviceDefaultCPU != "100m" || cfg.DeviceDefaultMemory != "256Mi" {
		t.Fatalf("defaults: %+v", cfg)
	}

	for name, kv := range map[string][2]string{
		"negative max pods":    {"SCHEDULER_MAX_PODS", "-1"},
		"platform reserve 100": {"SCHEDULER_PLATFORM_RESERVE_PERCENT", "100"},
		"reserve cpu -1":       {"SCHEDULER_PLATFORM_RESERVE_CPU", "-1"},
		"bad cpu default":      {"DEVICE_DEFAULT_CPU", "lots"},
		"bad startup timeout":  {"SCHEDULER_STARTUP_TIMEOUT", "soon"},
		"zero timeout":         {"SCHEDULER_STARTUP_TIMEOUT", "0s"},
		"zero threshold":       {"SCHEDULER_RESTART_THRESHOLD", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("%s=%s must be refused", kv[0], kv[1])
			}
		})
	}
}

// A moving image reference stops the operator: no tag, the tag latest. An exact tag and a digest start it.
func TestLoadConfigRefusesMovingImages(t *testing.T) {
	for image, ok := range map[string]bool{
		"registry.example.com/lab:v1.2.3":           true,
		"registry.example.com:5000/lab:0.1.0":       true,
		"registry.example.com/lab@sha256:abcdef":    true,
		"registry.example.com/lab:v1@sha256:abcdef": true,
		"registry.example.com/lab:latest":           false,
		"registry.example.com/lab":                  false,
		"registry.example.com:5000/lab":             false,
		"registry.example.com/lab:":                 false,
	} {
		for _, key := range []string{"VPN_IMAGE", "GATEWAY_IMAGE", "NETCONFIG_IMAGE"} {
			t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
			t.Setenv("BASE_DOMAIN", "labs.example.com")
			setRequiredEnv(t)
			t.Setenv(key, image)
			if _, err := LoadConfig(); (err == nil) != ok {
				t.Errorf("%s=%s: err %v, want ok=%v", key, image, err, ok)
			}
		}
	}
}

// setRequiredEnv sets what the chart always passes and the operator refuses to start without.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SUPPORT_EMAIL", "support@example.com")
	t.Setenv("VPN_IMAGE", "registry.example.com/lab:v1")
	t.Setenv("GATEWAY_IMAGE", "registry.example.com/lab:v1")
	t.Setenv("NETCONFIG_IMAGE", "registry.example.com/node:v1")
}

// There is no default for the images and the support address: an empty value stops the operator. The gateway is the VPN image
// (one image, two commands), so GATEWAY_IMAGE may be left out.
func TestLoadConfigRefusesEmptyImagesAndSupportEmail(t *testing.T) {
	for _, key := range []string{"VPN_IMAGE", "NETCONFIG_IMAGE", "SUPPORT_EMAIL"} {
		t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
		t.Setenv("BASE_DOMAIN", "labs.example.com")
		setRequiredEnv(t)
		if _, err := LoadConfig(); err != nil {
			t.Fatalf("all set: %v", err)
		}
		t.Setenv(key, "")
		if _, err := LoadConfig(); err == nil {
			t.Errorf("an empty %s must be refused", key)
		}
	}
}

func TestDeviceSecurityDefaults(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DeviceUserNamespaces || cfg.DeviceEphemeralStorage != "2Gi" {
		t.Fatalf("%v %q", cfg.DeviceUserNamespaces, cfg.DeviceEphemeralStorage)
	}
	t.Setenv("DEVICE_EPHEMERAL_STORAGE", "0")
	if cfg, err = LoadConfig(); err != nil || cfg.DeviceEphemeralStorage != "" {
		t.Fatalf("0 means no limit: %q %v", cfg.DeviceEphemeralStorage, err)
	}
	t.Setenv("DEVICE_EPHEMERAL_STORAGE", "lots")
	if _, err = LoadConfig(); err == nil {
		t.Fatal("a bad quantity must be refused")
	}
}

func TestLoadConfigGatewayImageDefaultsToTheVPNImage(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	setRequiredEnv(t)
	t.Setenv("GATEWAY_IMAGE", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayImage != cfg.VPNImage {
		t.Errorf("GatewayImage %q, want the VPN image %q", cfg.GatewayImage, cfg.VPNImage)
	}
}

// The binary defaults are the safe ones the chart also has: confined, with the agent and proxy bindings, and the registry derived.
func TestLoadConfigSafeDefaults(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireAdmissionPolicy || !cfg.AgentEnabled || !cfg.ProxyEnabled {
		t.Errorf("RequireAdmissionPolicy %v AgentEnabled %v ProxyEnabled %v: all must default to true", cfg.RequireAdmissionPolicy, cfg.AgentEnabled, cfg.ProxyEnabled)
	}
	if cfg.State.RegistryAddr != "laboratory-registry.laboratory-system.svc:5000" || cfg.Cache.Prefix != "localhost:5035" {
		t.Errorf("registry addresses: %q %q", cfg.State.RegistryAddr, cfg.Cache.Prefix)
	}
}

// The tuning knobs are optional environment variables with fixed defaults: nothing is required, any can be overridden without a rebuild.
func TestLoadConfigTuningKnobs(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VPNStatsInterval != 30*time.Second || cfg.AdmissionPolicyTimeout != 90*time.Second {
		t.Errorf("VPN_STATS_INTERVAL %v, OPERATOR_ADMISSION_POLICY_TIMEOUT %v", cfg.VPNStatsInterval, cfg.AdmissionPolicyTimeout)
	}
	if cfg.AgentServiceAccount != "laboratory-agent" || cfg.AgentServiceNamespace != "laboratory-agent" ||
		cfg.OperatorServiceAccount != "laboratory-controller-manager" || cfg.OperatorNamespace != "laboratory-system" {
		t.Errorf("identities: %+v", cfg)
	}
	if cfg.PublicVPNEndpoint != "vpn.example.com:51820" {
		t.Errorf("a host without a port is advertised with the default port: %q", cfg.PublicVPNEndpoint)
	}

	t.Setenv("VPN_STATS_INTERVAL", "7s")
	t.Setenv("OPERATOR_ADMISSION_POLICY_TIMEOUT", "5s")
	t.Setenv("AGENT_SERVICE_ACCOUNT", "agent-sa")
	t.Setenv("AGENT_SERVICE_NAMESPACE", "agent-ns")
	t.Setenv("OPERATOR_SERVICE_ACCOUNT", "op-sa")
	t.Setenv("OPERATOR_NAMESPACE", "op-ns")
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:443")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VPNStatsInterval != 7*time.Second || cfg.AdmissionPolicyTimeout != 5*time.Second || cfg.AgentServiceAccount != "agent-sa" ||
		cfg.AgentServiceNamespace != "agent-ns" || cfg.OperatorServiceAccount != "op-sa" || cfg.OperatorNamespace != "op-ns" {
		t.Errorf("overrides are not read: %+v", cfg)
	}
	if cfg.PublicVPNEndpoint != "vpn.example.com:443" {
		t.Errorf("an endpoint with a port is advertised as it is: %q", cfg.PublicVPNEndpoint)
	}
}
