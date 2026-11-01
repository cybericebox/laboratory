package chart_test

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
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
	if strings.Contains(on, "L7_PROXY_PROTOCOL_SECRETS") {
		t.Error("no secretRef: no secret is required")
	}
	withSecret, err := helmTemplate(t, "--set", "proxy.l7.proxyProtocol.trustedFrom={10.0.0.1/32}", "--set", "proxy.l7.proxyProtocol.secretRef.name=edge-pp", "--set", "proxy.l7.proxyProtocol.secretRef.key=secrets", "-s", "templates/proxy/deployment.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, withSecret)
	}
	for _, want := range []string{"L7_PROXY_PROTOCOL_SECRETS", "secretKeyRef", "name: edge-pp", "key: secrets", `"224"`} {
		if !strings.Contains(withSecret, want) {
			t.Errorf("the secret wiring lacks %q", want)
		}
	}
	if !strings.Contains(on, "L7_PROXY_PROTOCOL_FROM") || !strings.Contains(on, `"10.0.0.1/32,10.0.0.2/32"`) {
		t.Errorf("the trust list does not reach the proxy:\n%s", on)
	}
}

// The bandwidth protection with the proxy off must not name the proxy namespace: it does not exist, and the Role there fails the install.
func TestBandwidthWithoutTheProxyNamesNoProxyNamespace(t *testing.T) {
	out, err := helmTemplate(t, "--set", "bandwidth.enabled=true", "--set", "proxy.enabled=false", "-s", "templates/operator/role-bandwidth.yaml")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "namespace: laboratory-proxy") {
		t.Errorf("a Role in the proxy namespace with the proxy off:\n%s", out)
	}
	cfg := operatorConfig(t, "--set", "bandwidth.enabled=true", "--set", "proxy.enabled=false")
	if strings.Contains(cfg["BANDWIDTH_ENTRY_NAMESPACES"], "laboratory-proxy") {
		t.Errorf("entry namespaces: %q", cfg["BANDWIDTH_ENTRY_NAMESPACES"])
	}
	on := operatorConfig(t, "--set", "bandwidth.enabled=true")
	if !strings.Contains(on["BANDWIDTH_ENTRY_NAMESPACES"], "laboratory-proxy") {
		t.Errorf("with the proxy on its namespace is an entry namespace: %q", on["BANDWIDTH_ENTRY_NAMESPACES"])
	}
}

// The same for the values.yaml of the chart: vpn.resources and inetGateway.resources (the size of a group made with no size) are within
// base + perUnit * groupLimits.maxUsers / maxLabs, the maximum the agent reports. The agent refuses to start otherwise, and
// the daemon plans no group at the default size.
func TestChartDefaultPodSizesFitTheReportedMaximum(t *testing.T) {
	raw, err := os.ReadFile("../../charts/laboratory/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		VPN struct {
			Resources struct{ CPU, Memory string }
			Sizing    struct{ BaseCpu, BaseMemory, PerUserCpu, PerUserMemory string }
		}
		InetGateway struct {
			Resources struct{ CPU, Memory string }
			Sizing    struct{ BaseCpu, BaseMemory, PerLabCpu, PerLabMemory string }
		}
		GroupLimits struct{ MaxUsers, MaxLabs int }
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	q := func(s string) *resource.Quantity {
		r, err := resource.ParseQuantity(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return &r
	}
	check := func(name, defCPU, defMem, baseCPU, baseMem, perCPU, perMem string, units int) {
		maxCPU := q(baseCPU).MilliValue() + q(perCPU).MilliValue()*int64(units)
		maxMem := q(baseMem).Value() + q(perMem).Value()*int64(units)
		if q(defCPU).MilliValue() > maxCPU || q(defMem).Value() > maxMem {
			t.Errorf("%s: the default size (%s, %s) is over the maximum at %d units (%dm, %d bytes)", name, defCPU, defMem, units, maxCPU, maxMem)
		}
	}
	check("vpn", v.VPN.Resources.CPU, v.VPN.Resources.Memory, v.VPN.Sizing.BaseCpu, v.VPN.Sizing.BaseMemory, v.VPN.Sizing.PerUserCpu, v.VPN.Sizing.PerUserMemory, v.GroupLimits.MaxUsers)
	check("gateway", v.InetGateway.Resources.CPU, v.InetGateway.Resources.Memory, v.InetGateway.Sizing.BaseCpu, v.InetGateway.Sizing.BaseMemory, v.InetGateway.Sizing.PerLabCpu, v.InetGateway.Sizing.PerLabMemory, v.GroupLimits.MaxLabs)
}
