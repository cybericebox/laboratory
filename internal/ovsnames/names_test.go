package ovsnames

import "testing"

func TestLabIfaceName_Short(t *testing.T) {
	if got := LabIfaceName("mylab"); got != "lab-mylab" {
		t.Errorf("got %q, want lab-mylab", got)
	}
}

func TestLabIfaceName_Long(t *testing.T) {
	name := LabIfaceName("very-long-lab-name-that-exceeds-limit")
	if len(name) > 15 {
		t.Errorf("LabIfaceName len = %d, want ≤15: %q", len(name), name)
	}
}

func TestLabGWIfaceName_Short(t *testing.T) {
	if got := LabGWIfaceName("mylab"); got != "gw-mylab" {
		t.Errorf("got %q, want gw-mylab", got)
	}
}

func TestLabGWIfaceName_Long(t *testing.T) {
	name := LabGWIfaceName("very-long-lab-name-that-exceeds-limit")
	if len(name) > 15 {
		t.Errorf("LabGWIfaceName len = %d, want ≤15: %q", len(name), name)
	}
}

func TestLabNames_NoCollision(t *testing.T) {
	name := "mylab"
	if LabIfaceName(name) == LabGWIfaceName(name) {
		t.Errorf("LabIfaceName and LabGWIfaceName must differ for %q", name)
	}
	longName := "very-long-lab-name-that-exceeds-limit"
	if LabIfaceName(longName) == LabGWIfaceName(longName) {
		t.Errorf("LabIfaceName and LabGWIfaceName must differ for long name %q", longName)
	}
}
