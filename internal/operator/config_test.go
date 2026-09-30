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
