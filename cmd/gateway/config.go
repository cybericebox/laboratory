//go:build linux

package main

import (
	"fmt"
	"os"
)

type Config struct {
	Namespace         string
	ExternalInterface string
}

func loadConfig() (*Config, error) {
	ns := os.Getenv("NAMESPACE")
	if ns == "" {
		return nil, fmt.Errorf("NAMESPACE env required")
	}
	iface := os.Getenv("EXTERNAL_INTERFACE")
	if iface == "" {
		iface = "eth0"
	}
	return &Config{
		Namespace:         ns,
		ExternalInterface: iface,
	}, nil
}
