package chart_test

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Google's passthrough load balancers deliver a packet with the load balancer's own address and port as its destination, not the node port
// (infrastructure cluster-layout 18.5, G1). The public entries (L7 proxy 443, VPN, the agent's public listener with exposure=open) therefore carry
// that address as an external IP of their Service, so Cilium serves address:port on the node. Empty by default: AWS and the local sites target
// the node port and set nothing.
func TestPublicEntriesCarryTheLoadBalancerAddress(t *testing.T) {
	open := []string{"--set", "agent.enabled=true", "--set", "exposure=open", "--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=198.51.100.0/24"}
	for _, c := range []struct {
		tpl string
		set []string
	}{
		{"templates/proxy/service.yaml", []string{"--set", "proxy.l7.service.externalIPs[0]=203.0.113.10"}},
		{"templates/wg/service.yaml", []string{"--set", "proxy.wg.service.externalIPs[0]=203.0.113.10"}},
		{"templates/agent/service.yaml", append(append([]string{}, open...), "--set", "agent.service.externalIPs[0]=203.0.113.10")},
	} {
		var svc corev1.Service
		render(t, c.tpl, &svc, c.set...)
		if !slices.Equal(svc.Spec.ExternalIPs, []string{"203.0.113.10"}) {
			t.Errorf("%s: externalIPs %v, want [203.0.113.10]", c.tpl, svc.Spec.ExternalIPs)
		}
		if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
			t.Errorf("%s: type %s, policy %s: the external IP is added to the NodePort entry, which keeps Local", c.tpl, svc.Spec.Type, svc.Spec.ExternalTrafficPolicy)
		}

		var plain corev1.Service
		render(t, c.tpl, &plain, func() []string {
			if strings.Contains(c.tpl, "agent") {
				return open
			}
			return nil
		}()...)
		if len(plain.Spec.ExternalIPs) != 0 {
			t.Errorf("%s: externalIPs %v by default, want none", c.tpl, plain.Spec.ExternalIPs)
		}
	}
}

// The registry gate never takes an external IP: its callers are lab nodes, and Cilium on a lab node would short-circuit the call to the registry
// pod (lab to lab-control pod traffic, which the firewall drops). On Google the gate's load balancer port is the node port instead (G2). A value
// that asks for one is refused at install time, like the agent's without exposure=open.
func TestRegistryGateHasNoExternalIP(t *testing.T) {
	out, err := helmTemplate(t, "--set", "registry.gate.service.externalIPs[0]=203.0.113.10", "-s", "templates/registry/service.yaml")
	if err == nil {
		t.Fatalf("rendered with registry.gate.service.externalIPs, want a refusal:\n%s", out)
	}
	if !strings.Contains(out, "registry.gate.service.externalIPs") {
		t.Errorf("refusal %q does not name registry.gate.service.externalIPs", out)
	}

	var svc corev1.Service
	render(t, "templates/registry/service.yaml", &svc)
	if len(svc.Spec.ExternalIPs) != 0 {
		t.Errorf("registry gate externalIPs %v by default, want none", svc.Spec.ExternalIPs)
	}
}

// An external IP is a frontend on every node: only one plain IPv4 address of a public listener, and the agent's only with its public listener.
func TestEntryExternalIPsAreRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		set  []string
		want string
	}{
		{"agent without exposure open", []string{"--set", "agent.enabled=true", "--set", "agent.service.externalIPs[0]=203.0.113.10"}, "agent.service.externalIPs is set but exposure is"},
		{"a CIDR", []string{"--set", "proxy.l7.service.externalIPs[0]=203.0.113.0/24"}, "proxy.l7.service.externalIPs"},
		{"a name", []string{"--set", "proxy.wg.service.externalIPs[0]=lab.example.com"}, "proxy.wg.service.externalIPs"},
		{"the wildcard", []string{"--set", "proxy.l7.service.externalIPs[0]=0.0.0.0"}, "proxy.l7.service.externalIPs"},
		{"loopback", []string{"--set", "proxy.l7.service.externalIPs[0]=127.0.0.1"}, "proxy.l7.service.externalIPs"},
		{"link-local metadata address", []string{"--set", "proxy.l7.service.externalIPs[0]=169.254.169.254"}, "proxy.l7.service.externalIPs"},
		{"link-local low end", []string{"--set", "proxy.wg.service.externalIPs[0]=169.254.0.1"}, "proxy.wg.service.externalIPs"},
		{"link-local high end", []string{"--set", "proxy.wg.service.externalIPs[0]=169.254.255.254"}, "proxy.wg.service.externalIPs"},
		{"multicast", []string{"--set", "proxy.l7.service.externalIPs[0]=224.0.0.1"}, "proxy.l7.service.externalIPs"},
		{"multicast top", []string{"--set", "proxy.l7.service.externalIPs[0]=239.255.255.255"}, "proxy.l7.service.externalIPs"},
		{"reserved 240/4", []string{"--set", "proxy.l7.service.externalIPs[0]=240.0.0.1"}, "proxy.l7.service.externalIPs"},
		{"broadcast", []string{"--set", "proxy.l7.service.externalIPs[0]=255.255.255.255"}, "proxy.l7.service.externalIPs"},
		{"agent multicast", []string{"--set", "agent.enabled=true", "--set", "exposure=open", "--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=198.51.100.0/24", "--set", "agent.service.externalIPs[0]=224.0.0.251"}, "agent.service.externalIPs"},
		{"an octet out of range", []string{"--set", "proxy.l7.service.externalIPs[0]=203.0.113.300"}, "proxy.l7.service.externalIPs"},
		{"a leading zero", []string{"--set", "proxy.l7.service.externalIPs[0]=203.0.113.010"}, "proxy.l7.service.externalIPs"},
		{"not a list", []string{"--set", "proxy.l7.service.externalIPs=203.0.113.10"}, "proxy.l7.service.externalIPs must be a list"},
		{"two addresses", []string{"--set", "proxy.l7.service.externalIPs[0]=203.0.113.10", "--set", "proxy.l7.service.externalIPs[1]=203.0.113.11"}, "at most one"},
	} {
		out, err := helmTemplate(t, append(c.set, "-s", "templates/proxy/service.yaml")...)
		if err == nil {
			t.Errorf("%s: rendered, want a refusal", c.name)
			continue
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: refusal %q does not name %q", c.name, out, c.want)
		}
	}
}

// The refused ranges are exact: the addresses just outside them are valid load balancer addresses.
func TestEntryExternalIPsJustOutsideTheRefusedRangesAreAccepted(t *testing.T) {
	for _, ip := range []string{"169.253.255.255", "169.255.0.1", "223.255.255.255", "1.1.1.1"} {
		var svc corev1.Service
		render(t, "templates/proxy/service.yaml", &svc, "--set", "proxy.l7.service.externalIPs[0]="+ip)
		if !slices.Equal(svc.Spec.ExternalIPs, []string{ip}) {
			t.Errorf("externalIPs %v for %s, want it accepted", svc.Spec.ExternalIPs, ip)
		}
	}
}
