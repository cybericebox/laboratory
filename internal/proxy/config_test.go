package proxy

import "testing"

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
