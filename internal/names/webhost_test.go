package names

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewWebCode(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]+$`)
	seen := map[string]bool{}
	for _, n := range []int{WebCodeLen, WebCodeMaxLen} {
		for i := 0; i < 200; i++ {
			code, err := NewWebCode(n)
			if err != nil || len(code) != n || !re.MatchString(code) {
				t.Fatalf("NewWebCode(%d) = %q, %v", n, code, err)
			}
			seen[code] = true
		}
	}
	if len(seen) < 100 {
		t.Fatalf("codes look non-random: %d distinct of 400", len(seen))
	}
}

func TestWebHostLabelIsAValidDNSLabel(t *testing.T) {
	if got := WebHostLabel("web", "k3x"); got != "web-k3x" {
		t.Fatalf("label = %q", got)
	}
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)
	host := WebHostLabel(strings.Repeat("d", MaxDeviceNameLen), strings.Repeat("z", WebCodeMaxLen))
	if !valid.MatchString(host) {
		t.Fatalf("%q is not a valid label", host)
	}
}

func TestMaxDeviceNameLenFitsOneDNSLabel(t *testing.T) {
	if MaxDeviceNameLen != 35 {
		t.Fatalf("MaxDeviceNameLen = %d", MaxDeviceNameLen)
	}
	host := WebHostLabel(strings.Repeat("d", MaxDeviceNameLen), strings.Repeat("z", WebCodeMaxLen))
	if len(host) != 40 {
		t.Fatalf("len = %d", len(host))
	}
}

func TestValidateDeviceName(t *testing.T) {
	for name, wantErr := range map[string]bool{
		"web":                   false,
		"a":                     false,
		"host-1":                false,
		strings.Repeat("d", 35): false,
		strings.Repeat("d", 36): true,
		"":                      true,
		"-web":                  true,
		"web-":                  true,
		"Web":                   true,
		"web_1":                 true,
		"web.1":                 true,
		"веб":                   true,
	} {
		if err := ValidateDeviceName(name); (err != nil) != wantErr {
			t.Errorf("ValidateDeviceName(%q) = %v, wantErr %v", name, err, wantErr)
		}
	}
}
