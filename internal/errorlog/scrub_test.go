package errorlog

import (
	"strings"
	"testing"
)

// Nothing that identifies a person, a tenant, a host or a secret survives Redact.
func TestRedactRemovesWhatMustNotLeave(t *testing.T) {
	jwt := "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJhY21lIn0.c2lnbmF0dXJlYXNkZg"
	cases := map[string][]string{
		"login failed password=hunter2 for user":                                   {"hunter2"},
		`connect to 10.12.34.56:6443 refused`:                                      {"10.12.34.56"},
		`dial tcp 192.168.0.9:5000: i/o timeout`:                                   {"192.168.0.9"},
		`neighbor fe80::1ff:fe23:4567:890a unreachable`:                            {"fe80::1ff:fe23:4567:890a"},
		`mapped ::ffff:10.0.0.1 failed`:                                            {"10.0.0.1"},
		`pull https://alice:s3cr3t@registry.example.com/v2/ failed`:                {"alice", "s3cr3t", "registry.example.com"},
		`mail to owner@example.org bounced`:                                        {"owner@example.org"},
		`token ` + jwt + ` rejected`:                                               {jwt, "eyJ"},
		`Authorization: Bearer abcdefghijklmnop12345 invalid`:                      {"abcdefghijklmnop12345"},
		`mac aa:bb:cc:dd:ee:ff conflict`:                                           {"aa:bb:cc:dd:ee:ff"},
		`image "ghcr.io/acme/secret-lab:1.2" not found`:                            {"acme", "secret-lab"},
		`lab team-alpha/web-1 failed`:                                              {"team-alpha", "web-1"},
		`namespace lg-team-alpha-1a2b3c4d is terminating`:                          {"team-alpha"},
		`digest sha256:0123456789abcdef0123456789abcdef0123456789abcdef`:           {"0123456789abcdef0123456789abcdef"},
		"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkq\n-----END PRIVATE KEY-----": {"MIIEvQ", "PRIVATE KEY-----\nMII"},
		`group 0198c0a4-7a41-7000-8000-000000000002 failed`:                        {"0198c0a4"},
		`{"api_key":"abc123secret","user":"x"}`:                                    {"abc123secret"},
	}
	for in, forbidden := range cases {
		out := Redact(in)
		for _, f := range forbidden {
			if strings.Contains(out, f) {
				t.Errorf("Redact(%q) = %q still holds %q", in, out, f)
			}
		}
	}
}

func TestRedactKeepsWhatWentWrong(t *testing.T) {
	out := Redact("reconcile failed: the server rejected our request (Forbidden)")
	if !strings.Contains(out, "reconcile failed") || !strings.Contains(out, "Forbidden") {
		t.Fatalf("the error itself must stay readable: %q", out)
	}
	if long := Redact(strings.Repeat("word ", 200)); len(long) > MaxSampleLen {
		t.Fatalf("too long: %d", len(long))
	}
}

// Occurrences that differ only in numbers, ids, names and addresses group together.
func TestNormalizeGroupsTheSameError(t *testing.T) {
	a := Normalize(`update lab team-a/web-1: conflict, resourceVersion 1234 (attempt 2) from 10.1.2.3:443`)
	b := Normalize(`update lab team-b/db-9: conflict, resourceVersion 99 (attempt 5) from 10.9.9.9:443`)
	if a != b {
		t.Fatalf("not grouped:\n%s\n%s", a, b)
	}
	if Normalize("disk full on node-a1b2c3d4e5f6") == Normalize("connection refused") {
		t.Fatal("different errors must stay different")
	}
	if Fingerprint("operator", "reconcile", a) != Fingerprint("operator", "reconcile", b) {
		t.Fatal("the fingerprint follows the normalized text")
	}
	if Fingerprint("operator", "reconcile", a) == Fingerprint("node-agent", "reconcile", a) {
		t.Fatal("the component is part of the fingerprint")
	}
}
