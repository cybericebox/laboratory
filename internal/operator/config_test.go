package operator

import "testing"

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
		cfg.DeviceDefaultCPU != "250m" || cfg.DeviceDefaultMemory != "256Mi" {
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

// setRequiredEnv sets what the chart always passes and the operator refuses to start without.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SUPPORT_EMAIL", "support@example.com")
	t.Setenv("VPN_IMAGE", "registry.example.com/lab:v1")
	t.Setenv("GATEWAY_IMAGE", "registry.example.com/lab:v1")
	t.Setenv("NETCONFIG_IMAGE", "registry.example.com/node:v1")
}

// There is no default for the images and the support address: an empty value stops the operator.
func TestLoadConfigRefusesEmptyImagesAndSupportEmail(t *testing.T) {
	for _, key := range []string{"VPN_IMAGE", "GATEWAY_IMAGE", "NETCONFIG_IMAGE", "SUPPORT_EMAIL"} {
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
