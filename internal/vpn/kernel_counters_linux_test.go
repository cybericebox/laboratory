//go:build linux

package vpn

import "testing"

func TestMissingKernelBindingIsNotHealthyZero(t *testing.T) {
	_, err := parseKernelCounters([]byte("*filter\nCOMMIT\n"), map[string]ForwardRule{"0123456789abcdef": {ClientName: "p1", LabName: "a"}})
	if err == nil {
		t.Fatal("missing active accounting reported as healthy zero")
	}
}
