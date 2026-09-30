package names

import (
	"regexp"
	"strings"
	"testing"
)

func TestWebHostLabelIsUniquePerLabAndAValidDNSLabel(t *testing.T) {
	a := WebHostLabel("c-11111111-1111-1111-1111-111111111111", "web")
	b := WebHostLabel("c-22222222-2222-2222-2222-222222222222", "web")
	c := WebHostLabel("c-11111111-1111-1111-1111-111111111111-g1", "web")
	if a == b || a == c {
		t.Fatalf("collision: %s %s %s", a, b, c)
	}
	if a != WebHostLabel("c-11111111-1111-1111-1111-111111111111", "web") {
		t.Fatal("not stable")
	}
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)
	for _, host := range []string{a, WebHostLabel("c-x", strings.Repeat("d", 80))} {
		if !valid.MatchString(host) {
			t.Fatalf("%q is not a valid label", host)
		}
	}
	if !strings.HasPrefix(a, "web-") || len(a) != len("web-")+labShortIDLen {
		t.Fatalf("shape: %s", a)
	}
}
