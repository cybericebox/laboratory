package main

import (
	"testing"
)

func TestParseDefaultAnnotation_ExplicitEmpty(t *testing.T) {
	// annotation="" (explicitly set) → skipDelegate=true
	skip, iface := parseDefaultAnnotation("", false)
	if !skip {
		t.Error("empty annotation should skip delegate")
	}
	_ = iface
}

func TestParseDefaultAnnotation_Eth0(t *testing.T) {
	// annotation="eth0" → skipDelegate=false, targetIface="eth0"
	skip, iface := parseDefaultAnnotation("eth0", false)
	if skip {
		t.Error("eth0 annotation should not skip")
	}
	if iface != "eth0" {
		t.Errorf("got iface=%q want eth0", iface)
	}
}

func TestParseDefaultAnnotation_Custom(t *testing.T) {
	// annotation="mgmt" → skipDelegate=false, targetIface="mgmt"
	skip, iface := parseDefaultAnnotation("mgmt", false)
	if skip {
		t.Error("custom annotation should not skip")
	}
	if iface != "mgmt" {
		t.Errorf("got iface=%q want mgmt", iface)
	}
}

func TestParseDefaultAnnotation_NoAnnotation(t *testing.T) {
	// noAnnotation=true → skipDelegate=false, targetIface="eth0"
	skip, iface := parseDefaultAnnotation("", true)
	if skip {
		t.Error("no annotation should not skip")
	}
	if iface != "eth0" {
		t.Errorf("got iface=%q want eth0", iface)
	}
}
