//go:build linux

package main

import (
	"fmt"
	"os"
)

type Config struct {
	NodeName string
	OVSSock  string
	GRPCSock string
	Bridge   string
	ProcRoot string
}

func loadConfig() (*Config, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME env required")
	}
	return &Config{
		NodeName: nodeName,
		OVSSock:  getEnvOr("OVS_SOCK", "/run/openvswitch/db.sock"),
		GRPCSock: getEnvOr("GRPC_SOCK", "/run/cybericebox/node-agent.sock"),
		Bridge:   "br-ovs",
		ProcRoot: getEnvOr("PROC_ROOT", "/proc"),
	}, nil
}

func getEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
