package names

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestTenantSegment(t *testing.T) {
	long := strings.Repeat("a", 64)
	for name, want := range map[string]string{
		"acme":           "acme",
		"default":        "default",
		"a":              "a",
		"team-42":        "team-42",
		"0day":           "0day",
		"platform":       "t-" + hash12("platform"),
		"Acme":           "t-" + hash12("Acme"),
		"a.b":            "t-" + hash12("a.b"),
		"a_b":            "t-" + hash12("a_b"),
		"-a":             "t-" + hash12("-a"),
		"a-":             "t-" + hash12("a-"),
		"a~b":            "t-" + hash12("a~b"),
		long:             "t-" + hash12(long),
		"":               "t-" + hash12(""),
		"t-0123456789ab": "t-" + hash12("t-0123456789ab"), // reserved: the shape of an encoded name
	} {
		if got := TenantSegment(name); got != want {
			t.Errorf("TenantSegment(%q) = %q, want %q", name, got, want)
		}
	}
	if s := TenantSegment(strings.Repeat("a", 63)); s != strings.Repeat("a", 63) {
		t.Errorf("63 characters are safe: %q", s)
	}
	for _, name := range []string{"acme", "platform", "Acme", "a.b", long, ""} {
		if !ValidSegment(TenantSegment(name)) {
			t.Errorf("the segment of %q is not a valid segment: %q", name, TenantSegment(name))
		}
	}
}

// hash12 is computed here independently of the function under test, so a change of the encoding is noticed.
func hash12(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:12]
}
