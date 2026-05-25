//go:build linux

package main

import (
	"os"
	"testing"
)

func TestLoadConfig_RequiresNodeName(t *testing.T) {
	os.Unsetenv("NODE_NAME")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error when NODE_NAME not set")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	os.Setenv("NODE_NAME", "worker-1")
	defer os.Unsetenv("NODE_NAME")

	cfg, err := loadConfig()
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
