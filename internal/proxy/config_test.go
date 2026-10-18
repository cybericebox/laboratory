package proxy

import (
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/proxy/demux/fastpath"
)

func TestLoadL7Config_ShortSessionSecretIsRefused(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "a")
	t.Setenv("TLS_KEY_PATH", "b")
	t.Setenv("BASE_DOMAIN", "example.com")
	t.Setenv("SESSION_SECRET", "too-short")
	if _, err := LoadL7Config(); err == nil {
		t.Fatal("a short SESSION_SECRET must be refused")
	}
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	if _, err := LoadL7Config(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadL7Config_BaseDomainIsRequired(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "a")
	t.Setenv("TLS_KEY_PATH", "b")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	for _, v := range []string{"unset", ""} {
		if v == "" {
			t.Setenv("BASE_DOMAIN", "")
		}
		if _, err := LoadL7Config(); err == nil {
			t.Fatalf("BASE_DOMAIN %q must be refused", v)
		}
	}
}

// The code default mirrors the chart value proxy.l7.listen (an unprivileged port).
func TestLoadL7Config_ListenDefaultMirrorsTheChart(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "a")
	t.Setenv("TLS_KEY_PATH", "b")
	t.Setenv("BASE_DOMAIN", "example.com")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	cfg, err := LoadL7Config()
	if err != nil || cfg.Listen != ":8443" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestWGConfigMode(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	t.Setenv("DEMUX_MODE", "")
	if _, err := LoadWGConfig(); err == nil {
		t.Fatal("the default mode tc needs NODE_NAME")
	}
	t.Setenv("NODE_NAME", "node-1")
	cfg, err := LoadWGConfig()
	if err != nil || cfg.Mode != "tc" || cfg.PinPath != fastpath.DefaultPinPath || cfg.ServiceName != "laboratory-proxy-wg" ||
		cfg.ServiceNamespace != "laboratory-wg" || cfg.MetricsAddr != ":8083" || cfg.Resync != 10*time.Second {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv("DEMUX_MODE", "xdp")
	if cfg, err := LoadWGConfig(); err != nil || cfg.Mode != "xdp" {
		t.Fatalf("xdp: %+v %v", cfg, err)
	}
	t.Setenv("DEMUX_MODE", "foo")
	if _, err := LoadWGConfig(); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
	t.Setenv("NODE_NAME", "")
	t.Setenv("DEMUX_MODE", "xdp")
	if _, err := LoadWGConfig(); err == nil {
		t.Fatal("xdp needs NODE_NAME")
	}
	t.Setenv("DEMUX_MODE", "userspace")
	if cfg, err := LoadWGConfig(); err != nil || cfg.Mode != "userspace" {
		t.Fatalf("userspace without NODE_NAME: %+v %v", cfg, err)
	}
}
