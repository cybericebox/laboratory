//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestPortKey_Length(t *testing.T) {
	key := portKey("labgroup-abc", "lab-web-sqli--router1-eth0--switch1", "eth0")
	if len(key) > 15 {
		t.Errorf("portKey len = %d, want ≤15: %q", len(key), key)
	}
	if !strings.HasPrefix(key, "p") {
		t.Errorf("portKey must start with 'p', got %q", key)
	}
}

func TestPortKey_Deterministic(t *testing.T) {
	k1 := portKey("ns", "conn", "eth0")
	k2 := portKey("ns", "conn", "eth0")
	if k1 != k2 {
		t.Errorf("portKey not deterministic: %q != %q", k1, k2)
	}
}

func TestPortKey_Unique(t *testing.T) {
	k1 := portKey("ns", "conn", "eth0")
	k2 := portKey("ns", "conn", "eth1")
	if k1 == k2 {
		t.Errorf("portKey collision for different interfaces")
	}
}

func TestGenevePortName_Length(t *testing.T) {
	name := genevePortName("192.168.1.100")
	if len(name) > 15 {
		t.Errorf("genevePortName len = %d, want ≤15: %q", len(name), name)
	}
}

