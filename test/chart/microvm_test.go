package chart_test

import (
	"strings"
	"testing"
)

// The device profiles are off by default: no RuntimeClass, no profile labels, microVM not offered, so a cluster that upgrades keeps
// scheduling as before.
func TestMicroVMProfileIsOffByDefault(t *testing.T) {
	cfg := operatorConfig(t)
	for k, want := range map[string]string{"PROFILE_LABELS_ENABLED": "false", "MICROVM_ENABLED": "false", "SCALE_UP_PROFILES": ""} {
		if got, ok := cfg[k]; !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, want)
		}
	}
	if out, err := helmTemplate(t, "-s", "templates/microvm/runtimeclass.yaml"); err == nil && strings.Contains(out, "kind: RuntimeClass") {
		t.Errorf("a disabled microVM profile renders a RuntimeClass:\n%s", out)
	}
}

// Enabled, the chart renders the RuntimeClass of the profile: the handler of the implementation, the overhead the scheduler adds to
// every microVM pod, and the node selector with the profile label and the implementation label. The operator and the agent get the
// same values from one map.
func TestMicroVMRuntimeClass(t *testing.T) {
	out, err := helmTemplate(t, "--set", "microvm.enabled=true", "--set", "microvm.overhead.memory=200Mi", "-s", "templates/microvm/runtimeclass.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{
		"kind: RuntimeClass", "name: cybericebox-microvm", `handler: "kata-clh-runtime-rs"`, `memory: "200Mi"`, `cpu: "50m"`,
		`cybericebox.com/profile-microvm: "true"`, `cybericebox.com/runtime-kata-clh: "true"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the RuntimeClass lacks %q:\n%s", want, out)
		}
	}
	cfg := operatorConfig(t, "--set", "microvm.enabled=true", "--set", "profileLabels.enabled=true", "--set", "scaleUpProfiles={container,microvm}", "--set", "microvm.overhead.memory=200Mi")
	for k, want := range map[string]string{
		"MICROVM_ENABLED": "true", "PROFILE_LABELS_ENABLED": "true", "MICROVM_RUNTIME_CLASS": "cybericebox-microvm",
		"MICROVM_OVERHEAD_MEMORY": "200Mi", "MICROVM_OVERHEAD_CPU": "50m", "SCALE_UP_PROFILES": "container,microvm",
		"MICROVM_IMPLEMENTATION_LABEL": "cybericebox.com/runtime-kata-clh",
	} {
		if cfg[k] != want {
			t.Errorf("%s = %q, want %q", k, cfg[k], want)
		}
	}
}

func TestL7ProxyProtocolTrustList(t *testing.T) {
	off, err := helmTemplate(t, "-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, off)
	}
	if strings.Contains(off, "L7_PROXY_PROTOCOL_FROM") {
		t.Error("the PROXY protocol is off by default")
	}
	on, err := helmTemplate(t, "--set", "proxy.l7.proxyProtocol.trustedFrom={10.0.0.1/32,10.0.0.2/32}", "-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, on)
	}
	for _, want := range []string{"L7_PROXY_PROTOCOL_SECRET\n", "L7_PROXY_PROTOCOL_SECRET_NEXT", "name: l7-proxy-protocol", "key: current", "key: next", "optional: true", `"225"`} {
		if !strings.Contains(on, want) {
			t.Errorf("the secret wiring lacks %q", want)
		}
	}
	if !strings.Contains(on, "L7_PROXY_PROTOCOL_FROM") || !strings.Contains(on, `"10.0.0.1/32,10.0.0.2/32"`) {
		t.Errorf("the trust list does not reach the proxy:\n%s", on)
	}
}
