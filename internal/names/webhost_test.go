package names

import (
	"regexp"
	"strings"
	"testing"
)

// The same vectors are asserted by the TypeScript encoder in exercises-frontend
// (src/lib/labId.test.ts); keep them in sync.
var labIDVectors = []struct{ uuid, id string }{
	{"00000000-0000-0000-0000-000000000000", "0000000000000000000000000"},
	{"00000000-0000-0000-0000-000000000001", "0000000000000000000000001"},
	{"00000000-0000-0000-0000-000000000024", "0000000000000000000000010"},
	{"7f3c9a2e-4b1d-4e8a-9c6f-2d5b8e1a0c47", "7j6eora4bm1lw7i6anorqigxz"},
	{"ffffffff-ffff-ffff-ffff-ffffffffffff", "f5lxx1zz5pnorynqglhzmsp33"},
}

func TestLabIDVectors(t *testing.T) {
	for _, v := range labIDVectors {
		got, err := LabID(v.uuid)
		if err != nil || got != v.id {
			t.Errorf("LabID(%s) = %q, %v; want %q", v.uuid, got, err, v.id)
		}
		back, ok := ParseLabID(v.id)
		if !ok || back != v.uuid {
			t.Errorf("ParseLabID(%s) = %q, %v; want %q", v.id, back, ok, v.uuid)
		}
	}
}

func TestParseLabIDRejectsJunk(t *testing.T) {
	for _, bad := range []string{"", "short", strings.Repeat("Z", 25), strings.Repeat("-", 25), strings.Repeat("z", 25), strings.Repeat("0", 26)} {
		if _, ok := ParseLabID(bad); ok {
			t.Errorf("ParseLabID(%q) accepted", bad)
		}
	}
}

func TestWebHostLabelIsUniquePerLabAndAValidDNSLabel(t *testing.T) {
	a := WebHostLabel("7f3c9a2e-4b1d-4e8a-9c6f-2d5b8e1a0c47", "web")
	b := WebHostLabel("00000000-0000-0000-0000-000000000001", "web")
	if a == b || a != "web-7j6eora4bm1lw7i6anorqigxz" {
		t.Fatalf("labels: %s %s", a, b)
	}
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)
	for _, host := range []string{a, WebHostLabel("7f3c9a2e-4b1d-4e8a-9c6f-2d5b8e1a0c47", strings.Repeat("d", MaxDeviceNameLen))} {
		if !valid.MatchString(host) {
			t.Fatalf("%q is not a valid label", host)
		}
	}
	id, ok := LabIDFromWebHostLabel(a)
	if !ok || id != "7j6eora4bm1lw7i6anorqigxz" {
		t.Fatalf("LabIDFromWebHostLabel = %q %v", id, ok)
	}
}

func TestMaxDeviceNameLenFillsOneDNSLabel(t *testing.T) {
	if MaxDeviceNameLen != 37 {
		t.Fatalf("MaxDeviceNameLen = %d", MaxDeviceNameLen)
	}
	host := WebHostLabel("7f3c9a2e-4b1d-4e8a-9c6f-2d5b8e1a0c47", strings.Repeat("d", MaxDeviceNameLen))
	if len(host) != 63 {
		t.Fatalf("len = %d", len(host))
	}
}

func TestValidateDeviceName(t *testing.T) {
	for name, wantErr := range map[string]bool{
		"web":                   false,
		"a":                     false,
		"host-1":                false,
		strings.Repeat("d", 37): false,
		strings.Repeat("d", 38): true,
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
