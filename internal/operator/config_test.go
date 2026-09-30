package operator

import "testing"

func TestLoadConfigRequiresLabDomains(t *testing.T) {
	set := func(vpn, base string) {
		t.Setenv("PUBLIC_VPN_ENDPOINT", vpn)
		t.Setenv("BASE_DOMAIN", base)
		t.Setenv("SUPPORT_EMAIL", "support@example.com")
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
	t.Setenv("SUPPORT_EMAIL", "support@example.com")
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
