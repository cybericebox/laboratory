package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type Config struct {
	PrivateKey    string
	ListenPort    int
	Namespace     string
	ClientSubnet  *net.IPNet // 10.8.0.0/24 — user IPs
	VPNSupernet   *net.IPNet // 10.8.0.0/16
	StatsInterval time.Duration
	WGInterface   string
}

func loadConfig() (*Config, error) {
	privKey := os.Getenv("PRIVATE_KEY")
	if privKey == "" {
		return nil, fmt.Errorf("PRIVATE_KEY env required")
	}
	ns := os.Getenv("NAMESPACE")
	if ns == "" {
		return nil, fmt.Errorf("NAMESPACE env required")
	}
	port := 51820
	if s := os.Getenv("LISTEN_PORT"); s != "" {
		var err error
		port, err = strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("invalid LISTEN_PORT: %w", err)
		}
	}
	statsInterval := 30 * time.Second
	if s := os.Getenv("STATS_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid STATS_INTERVAL: %w", err)
		}
		statsInterval = d
	}
	_, clientSubnet, err := net.ParseCIDR("10.8.0.0/24")
	if err != nil {
		return nil, fmt.Errorf("parse clientSubnet: %w", err)
	}
	_, vpnSupernet, err := net.ParseCIDR("10.8.0.0/16")
	if err != nil {
		return nil, fmt.Errorf("parse vpnSupernet: %w", err)
	}
	return &Config{
		PrivateKey:    privKey,
		ListenPort:    port,
		Namespace:     ns,
		ClientSubnet:  clientSubnet,
		VPNSupernet:   vpnSupernet,
		StatsInterval: statsInterval,
		WGInterface:   "wg0",
	}, nil
}
