package chart_test

import (
	"strings"
	"testing"
)

func rendersWG(t *testing.T, extra ...string) bool {
	t.Helper()
	out, err := helmTemplate(t, append(extra, "-s", "templates/wg/daemonset.yaml")...)
	if err != nil {
		return false // helm says the template renders nothing
	}
	return strings.Contains(out, "kind: DaemonSet")
}

// The VPN switch owns the WireGuard demultiplexer, and the proxy switch no longer does.
func TestVPNSwitchOwnsTheDemux(t *testing.T) {
	if !rendersWG(t) {
		t.Error("by default the demux runs")
	}
	if rendersWG(t, "--set", "vpn.enabled=false") {
		t.Error("vpn.enabled=false: no demux")
	}
	if !rendersWG(t, "--set", "proxy.enabled=false") {
		t.Error("the proxy being off does not take the VPN away")
	}
}

// The switches reach the operator and the agent through the shared environment, with the defaults the docs promise.
func TestFeatureSwitchesReachTheComponents(t *testing.T) {
	env := operatorConfig(t)
	for k, want := range map[string]string{"VPN_ENABLED": "true", "INTERNET_ENABLED": "true", "PROXY_L7_ENABLED": "true", "PROXY_RDP_ENABLED": "false", "PROXY_VNC_ENABLED": "false"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q (%v)", k, env[k], want, env)
		}
	}
	env = operatorConfig(t, "--set", "vpn.enabled=false", "--set", "inetGateway.enabled=false", "--set", "proxy.l7.enabled=false")
	for _, k := range []string{"VPN_ENABLED", "INTERNET_ENABLED", "PROXY_L7_ENABLED"} {
		if env[k] != "false" {
			t.Errorf("%s = %q, want false", k, env[k])
		}
	}
}

// RDP and VNC are served by the L7 proxy: turning one on without the proxy is a mistake the chart refuses. Both are off by default, so
// the chart with the proxy off still renders (every access is refused anyway).
func TestRDPAndVNCNeedTheL7Proxy(t *testing.T) {
	if out, err := helmTemplate(t, "--set", "proxy.l7.enabled=false"); err != nil {
		t.Errorf("the defaults must not break a chart without the proxy: %v\n%s", err, out)
	}
	for _, set := range []string{"proxy.rdp.enabled", "proxy.vnc.enabled"} {
		if out, err := helmTemplate(t, "--set", set+"=true", "--set", "proxy.l7.enabled=false"); err == nil {
			t.Errorf("%s without the L7 proxy must fail to render", set)
		} else if !strings.Contains(out, "proxy") {
			t.Errorf("%s: the message must name the proxy: %s", set, out)
		}
		if out, err := helmTemplate(t, "--set", set+"=true"); err != nil {
			t.Errorf("%s with the proxy: %v\n%s", set, err, out)
		}
	}
}

// With a switch off, its admission rule is rendered; with everything on there are no feature rules to render.
func TestFeaturePolicyFollowsTheSwitches(t *testing.T) {
	render := func(extra ...string) string {
		out, err := helmTemplate(t, append(extra, "-s", "templates/operator/feature-policy.yaml")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	off := render("--set", "vpn.enabled=false", "--set", "inetGateway.enabled=false", "--set", "proxy.l7.enabled=false")
	for _, reason := range []string{"VPN_UNAVAILABLE", "INTERNET_UNAVAILABLE", "ACCESS_UNAVAILABLE"} {
		if !strings.Contains(off, reason) {
			t.Errorf("%s is not in the rules of a laboratory with the features off", reason)
		}
	}
	on := render("--set", "proxy.rdp.enabled=true", "--set", "proxy.vnc.enabled=true", "--set", "terminal.enabled=true", "--set", "checks.enabled=true")
	for _, reason := range []string{"VPN_UNAVAILABLE", "INTERNET_UNAVAILABLE", "ACCESS_UNAVAILABLE", "CHECKS_UNAVAILABLE"} {
		if strings.Contains(on, reason) {
			t.Errorf("%s must not be refused while the feature is served", reason)
		}
	}
	if !strings.Contains(on, "GROUP_VPN_OFF") || !strings.Contains(on, "GROUP_INTERNET_OFF") {
		t.Error("the group rules stay: a group may switch its servers off whatever the cluster serves")
	}
}

// registry.allowedRegistries reaches the components with the registries of the platform's own images added, "*" wins, and the pod policy
// carries the same list.
func TestAllowedRegistriesReachTheComponents(t *testing.T) {
	if got := operatorConfig(t)["ALLOWED_REGISTRIES"]; got != "docker.io,ghcr.io,quay.io,registry.k8s.io" {
		t.Errorf("default list: %q", got)
	}
	got := operatorConfig(t, "--set", "registry.allowedRegistries={Index.docker.io,reg.example.com:5000}",
		"--set", "vpn.image.repository=ghcr.io/own/lab", "--set", "nodeAgent.image.repository=own.example.com/node")["ALLOWED_REGISTRIES"]
	if got != "docker.io,ghcr.io,own.example.com,reg.example.com:5000" {
		t.Errorf("own registries are added and the Hub spelling is docker.io: %q", got)
	}
	if got := operatorConfig(t, "--set", "registry.allowedRegistries={docker.io,*}")["ALLOWED_REGISTRIES"]; got != "*" {
		t.Errorf("star: %q", got)
	}
	policy := func(extra ...string) string {
		out, err := helmTemplate(t, append(extra, "-s", "templates/operator/admission-policy.yaml")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	if p := policy(); !strings.Contains(p, "REGISTRY_NOT_ALLOWED") || !strings.Contains(p, "'ghcr.io'") || !strings.Contains(p, "localhost:5035") {
		t.Error("the pod policy must carry the list and the node's forwarder")
	}
	if p := policy("--set", "registry.allowedRegistries={*}"); strings.Contains(p, "REGISTRY_NOT_ALLOWED") {
		t.Error("with * there is no image rule")
	}
}

// The demultiplexer is rendered by the VPN switch alone, so a cluster that runs the VPN without the proxy gets its objects. Its Cilium
// policy follows the network policy switch of the proxy section (proxy.networkPolicy.enabled) like the policies of every other component,
// and proxy.wg.enabled=false takes the whole demux away: a cluster with no Cilium (Kind) installs with either.
func TestVPNWithoutTheProxyRendersNoCiliumPolicyWhenPoliciesAreOff(t *testing.T) {
	cilium := func(extra ...string) int {
		out, err := helmTemplate(t, append([]string{"--set", "proxy.enabled=false"}, extra...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		n := 0
		for _, d := range docs(t, out) {
			if d["kind"] == "CiliumNetworkPolicy" {
				if md, _ := d["metadata"].(map[string]any); md["name"] == "laboratory-proxy-wg" {
					n++
				}
			}
		}
		return n
	}
	if cilium() != 1 {
		t.Error("vpn on, proxy off, policies on: the demux has its Cilium policy")
	}
	if cilium("--set", "proxy.networkPolicy.enabled=false") != 0 {
		t.Error("policies off: no CiliumNetworkPolicy for the demux")
	}
	if cilium("--set", "proxy.wg.enabled=false") != 0 {
		t.Error("proxy.wg.enabled=false: no demux, no policy")
	}
	if cilium("--set", "vpn.enabled=false") != 0 {
		t.Error("vpn off: no demux, no policy")
	}
}
