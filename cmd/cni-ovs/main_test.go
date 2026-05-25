package main

import (
	"reflect"
	"testing"
)

func TestParseNetworksAnnotation_Empty(t *testing.T) {
	got := parseNetworksAnnotation("")
	if got != nil {
		t.Errorf("expected nil for empty annotation, got %+v", got)
	}
}

func TestParseNetworksAnnotation_Single(t *testing.T) {
	got := parseNetworksAnnotation("conn-a@eth0")
	want := []netAttachment{{Connection: "conn-a", Interface: "eth0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestParseNetworksAnnotation_Multiple(t *testing.T) {
	got := parseNetworksAnnotation("conn-a@eth0,conn-b@eth1")
	want := []netAttachment{
		{Connection: "conn-a", Interface: "eth0"},
		{Connection: "conn-b", Interface: "eth1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}
