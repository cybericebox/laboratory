package chart_test

import (
	"strings"
	"testing"
)

// The Gateway has only the TLS passthrough listener by default; gateway.extraListeners appends listeners after it.
func TestGatewayExtraListeners(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/gateway/gateway.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Count(out, "- name:") != 1 || !strings.Contains(out, "name: tls-passthrough") {
		t.Fatalf("default Gateway must have the one passthrough listener:\n%s", out)
	}
	out, err = helmTemplate(t, "-s", "templates/gateway/gateway.yaml",
		"--set", "gateway.extraListeners[0].name=h3", "--set", "gateway.extraListeners[0].protocol=UDP",
		"--set", "gateway.extraListeners[0].port=8443")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tls, h3 := strings.Index(out, "name: tls-passthrough"), strings.Index(out, "name: h3")
	if tls < 0 || h3 < tls || !strings.Contains(out, "protocol: UDP") || !strings.Contains(out, "port: 8443") {
		t.Fatalf("the extra listener must follow tls-passthrough:\n%s", out)
	}
}
