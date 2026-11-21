package proxy

import (
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/proxy/demux/fastpath"
)

// The proxy holds no secret of its own: the grant settings are a lifetime bound, the cookie name and the pages that may post a grant.
func TestLoadL7Config_GrantSettings(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "a")
	t.Setenv("TLS_KEY_PATH", "b")
	t.Setenv("BASE_DOMAIN", "example.com")
	cfg, err := LoadL7Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GrantMaxTTL != 8*time.Hour || cfg.CookieName != "__Host-cib_grant" || len(cfg.PlatformOrigins) != 0 {
		t.Fatalf("defaults: %v %q %v", cfg.GrantMaxTTL, cfg.CookieName, cfg.PlatformOrigins)
	}
	t.Setenv("PLATFORM_ORIGINS", "https://app.example.com,https://*.example.org")
	t.Setenv("GRANT_MAX_TTL", "2h")
	t.Setenv("GRANT_COOKIE_NAME", "g")
	cfg, err = LoadL7Config()
	if err != nil || cfg.GrantMaxTTL != 2*time.Hour || cfg.CookieName != "g" || len(cfg.PlatformOrigins) != 2 || cfg.PlatformOrigins[1] != "https://*.example.org" {
		t.Fatalf("%+v %v", cfg, err)
	}
	t.Setenv("GRANT_MAX_TTL", "0s")
	if _, err := LoadL7Config(); err == nil {
		t.Fatal("a zero grant lifetime must be refused")
	}
}

func TestLoadL7Config_BaseDomainIsRequired(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "a")
	t.Setenv("TLS_KEY_PATH", "b")
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
	cfg, err := LoadL7Config()
	if err != nil || cfg.Listen != ":8443" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestWGConfigMode(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	t.Setenv("DEMUX_MODE", "")
	if _, err := LoadWGConfig(); err != nil {
		t.Fatalf("the default mode userspace needs no NODE_NAME: %v", err)
	}
	t.Setenv("DEMUX_MODE", "tc")
	if _, err := LoadWGConfig(); err == nil {
		t.Fatal("the tc mode needs NODE_NAME")
	}
	t.Setenv("DEMUX_MODE", "")
	t.Setenv("NODE_NAME", "node-1")
	cfg, err := LoadWGConfig()
	if err != nil || cfg.Mode != "userspace" || cfg.PinPath != fastpath.DefaultPinPath || cfg.ServiceName != "laboratory-proxy-wg" ||
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
