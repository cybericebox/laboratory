package operator

import "testing"

func TestStateConfigDefaults(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "lab.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	st := cfg.State
	if st.Enabled {
		t.Fatal("state persistence is off by default")
	}
	if st.Debounce.String() != "5s" || st.MaxLayers != 10 || st.Retention.String() != "168h0m0s" {
		t.Fatalf("defaults: %+v", st)
	}
	if got := len(st.ExcludePaths); got != 3 || st.ExcludePaths[0] != "/tmp" {
		t.Fatalf("exclude paths: %v", st.ExcludePaths)
	}
	if n, err := st.WriteQuotaBytes(); err != nil || n != 512<<20 {
		t.Fatalf("max snapshot size: %d %v", n, err)
	}
}

func TestStateConfigFromEnv(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "lab.example.com")
	setRequiredEnv(t)
	t.Setenv("STATE_PERSISTENCE_ENABLED", "true")
	t.Setenv("STATE_DEBOUNCE", "12s")
	t.Setenv("STATE_EXCLUDE_PATHS", "/cache,/var/log")
	t.Setenv("STATE_WRITE_QUOTA", "1Gi")
	t.Setenv("STATE_MAX_LAYERS", "4")
	t.Setenv("STATE_RETENTION", "24h")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	st := cfg.State
	if !st.Enabled || st.Debounce.Seconds() != 12 || st.MaxLayers != 4 || st.Retention.Hours() != 24 ||
		len(st.ExcludePaths) != 2 || st.ExcludePaths[1] != "/var/log" {
		t.Fatalf("%+v", st)
	}
	if n, _ := st.WriteQuotaBytes(); n != 1<<30 {
		t.Fatalf("max snapshot size %d", n)
	}
}

func TestStateConfigRejectsBadQuota(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "lab.example.com")
	setRequiredEnv(t)
	t.Setenv("STATE_WRITE_QUOTA", "lots")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("an unparseable quota must fail at startup")
	}
}

func TestCacheConfig(t *testing.T) {
	t.Setenv("PUBLIC_VPN_ENDPOINT", "vpn.example.com:51820")
	t.Setenv("BASE_DOMAIN", "lab.example.com")
	setRequiredEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Enabled || cfg.Cache.Rewriter().Prefix != "" {
		t.Fatal("the image cache is off by default and rewrites nothing")
	}
	t.Setenv("IMAGE_CACHE_ENABLED", "true")
	t.Setenv("IMAGE_CACHE_REGISTRIES", "docker.io,registry.example.com")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	rw := cfg.Cache.Rewriter()
	if rw.Prefix != "localhost:5035" || len(rw.Registries) != 2 {
		t.Fatalf("%+v", rw)
	}
	if got := rw.Rewrite("nginx"); got != "localhost:5035/docker.io/library/nginx:latest" {
		t.Fatal(got)
	}
}
