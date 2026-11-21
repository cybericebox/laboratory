//go:build linux

package nodeagent

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig_RequiresNodeName(t *testing.T) {
	os.Unsetenv("NODE_NAME")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error when NODE_NAME not set")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	os.Setenv("NODE_NAME", "worker-1")
	defer os.Unsetenv("NODE_NAME")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NodeName != "worker-1" {
		t.Errorf("NodeName = %q, want %q", cfg.NodeName, "worker-1")
	}
	if cfg.Bridge != "br-ovs" {
		t.Errorf("Bridge = %q, want %q", cfg.Bridge, "br-ovs")
	}
	if cfg.GRPCSock != "/run/cybericebox/node-agent.sock" {
		t.Errorf("GRPCSock = %q", cfg.GRPCSock)
	}
}

// The paths, the bridge and the pull settings are optional environment variables with fixed defaults; the registry relay port is not.
func TestLoadConfig_TuningKnobs(t *testing.T) {
	t.Setenv("NODE_NAME", "worker-1")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunCheckPath != "/sys/class/misc/tun/dev" || cfg.CgroupRoot != "/host/sys/fs/cgroup" || cfg.StateWorkDir != "/var/cache/cybericebox/state" ||
		cfg.ImagePullConcurrency != 2 || cfg.ImagePullTimeout != 5*time.Minute {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.StateForwardPort != 5035 || cfg.StateRegistryAddr != "laboratory-registry.laboratory-system.svc:5443" {
		t.Errorf("registry: relay port %d, address %q", cfg.StateForwardPort, cfg.StateRegistryAddr)
	}
	t.Setenv("TUN_CHECK_PATH", "/x/tun")
	t.Setenv("CGROUP_ROOT", "/x/cgroup")
	t.Setenv("STATE_WORK_DIR", "/x/state")
	t.Setenv("IMAGE_PULL_CONCURRENCY", "6")
	t.Setenv("IMAGE_PULL_TIMEOUT", "30s")
	t.Setenv("STATE_REGISTRY_ADDR", "reg.example:5000")
	t.Setenv("STATE_FORWARD_PORT", "6000")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TunCheckPath != "/x/tun" || cfg.CgroupRoot != "/x/cgroup" || cfg.StateWorkDir != "/x/state" || cfg.ImagePullConcurrency != 6 ||
		cfg.ImagePullTimeout != 30*time.Second || cfg.StateRegistryAddr != "reg.example:5000" {
		t.Errorf("overrides are not read: %+v", cfg)
	}
	if cfg.StateForwardPort != 5035 {
		t.Errorf("the relay port is a constant, not an input: %d", cfg.StateForwardPort)
	}
}

// With no platform registry in the chart (REGISTRY_ENABLED=false) the node-agent has no registry address, so SetupDeviceState starts
// no relay; state persistence cannot be on without one.
func TestLoadConfig_NoRegistry(t *testing.T) {
	t.Setenv("NODE_NAME", "worker-1")
	t.Setenv("REGISTRY_ENABLED", "false")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateRegistryAddr != "" || cfg.RegistryEnabled {
		t.Errorf("no registry: address %q, enabled %v", cfg.StateRegistryAddr, cfg.RegistryEnabled)
	}
	t.Setenv("STATE_REGISTRY_ADDR", "reg.example:5000")
	if cfg, err = LoadConfig(); err != nil || cfg.StateRegistryAddr != "" {
		t.Errorf("an address is ignored without a registry: %q, %v", cfg.StateRegistryAddr, err)
	}
	t.Setenv("STATE_PERSISTENCE_ENABLED", "true")
	if _, err = LoadConfig(); err == nil {
		t.Error("state persistence without a registry must be refused")
	}
}
